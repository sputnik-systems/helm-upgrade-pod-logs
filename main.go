/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"time"

	"k8s.io/klog/v2"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"

	v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	typed_core_v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
}

// NewController creates a new Controller.
func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.getPodLogs(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

func (c *Controller) getPodLogs(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		log.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if exists {
		pod := obj.(*v1.Pod)

		podStartTime := pod.Status.StartTime
		if podStartTime != nil {
			if timeStart.isValidLag(podStartTime.Unix(), timeLag) {
				events, err := eventsGetter.List(context.TODO(), meta_v1.ListOptions{})
				if err != nil {
					log.Error(err)
				} else {
					for _, event := range events.Items {
						if event.InvolvedObject.Name == pod.Name && event.Type == "Warning" && event.LastTimestamp.Unix() >= podStartTime.Unix() {
							log.Infof("%s %s %s", event.Type, event.Reason, event.Message)
						}
					}
				}

				containerStatuses := pod.Status.ContainerStatuses
				for _, status := range containerStatuses {
					state := status.State
					if state.Terminated != nil {
						if state.Terminated.Reason != "Completed" {
							stream, err := podsGetter.GetLogs(pod.Name, logOptions).Stream(context.TODO())
							if err != nil {
								return err
							}
							defer stream.Close()

							scanner := bufio.NewScanner(stream)

							log.Infof("container %s in pod %s failed", status.Name, pod.Name)
							for scanner.Scan() {
								fmt.Println(scanner.Text())
							}
						}
					}
				}
			}
		}
	}

	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping pod %q out of the queue: %v", key, err)
}

// Run begins watching and syncing.
func (c *Controller) Run(ctx context.Context, threadiness int) {
	defer runtime.HandleCrash()

	defer c.queue.ShutDown()

	go c.informer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	go wait.UntilWithContext(ctx, c.runWorker, time.Second)

	ch := make(chan struct{})
	go runHelmChecker(ch)

	log.Info("started helm deploy checker")

	select {
	case <-ctx.Done():
		log.Info("ended helm deploy checker by timeout")
		return
	case <-ch:
		log.Info("ended helm deploy checker")
		return
	}
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem() {
	}
}

func runHelmChecker(ch chan struct{}) {
	var value struct{}

	settings := cli.New()
	cfg := new(action.Configuration)
	cfg.Init(settings.RESTClientGetter(), cliFlags.namespace, os.Getenv("HELM_DRIVER"), nil)

	list := action.NewList(cfg)
	list.All = true
	list.SetStateMask()

	for {
		releases, err := list.Run()
		if err != nil {
			log.Info(err)
		} else {
			for _, release := range releases {
				if release.Name == cliFlags.helmReleaseName {
					if release.Info.Status != "pending-install" &&
						release.Info.Status != "pending-upgrade" {
						if timeStart.isDeployOutdated(timeLag) ||
							timeStart.isValidLag(release.Info.LastDeployed.Unix(), timeLag) {
							break
						}

						ch <- value
					}
				}
			}
		}

		time.Sleep(5 * time.Second)
	}
}

func (t timestamp) isDeployOutdated(l float64) bool {
	now := time.Now().Unix()

	log.Debugf("start time: %d, current time: %d, delta: %d, lag time: %f", t, now, now-int64(t), l)

	return float64(now-int64(t)) <= l
}

func (t timestamp) isValidLag(e int64, l float64) bool {
	log.Debugf("start time: %d, event time: %d, delta: %d, lag time: %f", t, e, e-int64(t), l)

	now := time.Now().Unix()

	switch {
	// don't check if passed less than "time lag" time
	case float64(now-int64(t)) <= l:
		return true
	// delta betwen event tima and current time
	// less than specified time lag
	case math.Abs(float64(now-e)) <= l:
		return true
	default:
		return false
	}
}

type flags struct {
	kubeconfig      string
	namespace       string
	timeout         string
	timeLag         string
	selector        string
	helmReleaseName string
}

// type params struct {
// 	timestamp timestamp
// 	timeout   time.Duration
// 	lag       float64
// }

type timestamp int64

var (
	cliFlags flags
	// cliParams    params
	podsGetter   typed_core_v1.PodInterface
	eventsGetter typed_core_v1.EventInterface
	logOptions   *v1.PodLogOptions = &v1.PodLogOptions{}
	selector                       = func(options *meta_v1.ListOptions) {
		labelSelector, _ := labels.Parse(cliFlags.selector)
		options.LabelSelector = labelSelector.String()
	}

	timeout   time.Duration
	timeStart timestamp
	timeLag   float64
)

var rootCmd = &cobra.Command{
	Use:  "helm-upgrade-pod-logs",
	Long: "Print new created pod logs if container goes in failed state",
	RunE: func(cmd *cobra.Command, args []string) error {
		return execute()
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})

		if cliFlags.kubeconfig == "" {
			cliFlags.kubeconfig = os.Getenv("KUBECONFIG")
		}

		var err error

		// cliParams.timeout, err = time.ParseDuration(cliFlags.timeout)
		timeout, err = time.ParseDuration(cliFlags.timeout)
		if err != nil {
			return err
		}

		lag, err := time.ParseDuration(cliFlags.timeLag)
		if err != nil {
			return err
		}

		// cliParams.lag = timeLag.Seconds()
		// cliParams.timestamp = timestamp(time.Now().Unix())
		timeLag = lag.Seconds()
		timeStart = timestamp(time.Now().Unix())

		if cliFlags.helmReleaseName == "" {
			labelSelector, _ := labels.Parse(cliFlags.selector)
			cliFlags.helmReleaseName, _ = labelSelector.RequiresExactMatch("app")
		}

		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cliFlags.kubeconfig, "kubeconfig", "", "path to kubeconfig")
	rootCmd.PersistentFlags().StringVar(&cliFlags.timeout, "timeout", "5m0s", "process timeout")
	rootCmd.PersistentFlags().StringVar(&cliFlags.timeLag, "time-lag", "30s", "max lag between proccess running time and kubernetes objects changed time")
	rootCmd.PersistentFlags().StringVarP(&cliFlags.namespace, "namespace", "n", "default", "namespace")
	rootCmd.PersistentFlags().StringVarP(&cliFlags.selector, "selector", "l", "", "Selector (label query) to filter on")
	rootCmd.PersistentFlags().StringVar(&cliFlags.helmReleaseName, "helm-release-name", "", "Helm release name for deploy state checking")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Error(err)
	}
}

func execute() error {
	kubeconfig, err := ioutil.ReadFile(cliFlags.kubeconfig)
	if err != nil {
		return err
	}

	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	podsGetter = clientset.CoreV1().Pods(cliFlags.namespace)
	eventsGetter = clientset.CoreV1().Events(cliFlags.namespace)

	podListWatcher := cache.NewFilteredListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", cliFlags.namespace, selector)

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	// ctx, cancel := context.WithTimeout(context.Background(), cliParams.timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	controller := NewController(queue, indexer, informer)
	controller.Run(ctx, 1)

	return nil
}
