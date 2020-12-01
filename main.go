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
	"os"
	// "flag"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"time"

	"k8s.io/klog/v2"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	// "github.com/spf13/viper"
	// "github.com/davecgh/go-spew/spew"

	v1 "k8s.io/api/core/v1"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	// meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// "k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
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

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		log.Infof("Pod %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		// fmt.Printf("Sync/Add/Update for Pod %s\n", obj.(*v1.Pod).GetName())

		pod := obj.(*v1.Pod)

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

					buf := new(strings.Builder)
					io.Copy(buf, stream)

					log.Info(buf.String())
				}
			}
		}
	}

	return nil
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncToStdout(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		fmt.Printf("Pod %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		fmt.Printf("Sync/Add/Update for Pod %s\n", obj.(*v1.Pod).GetName())
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
	log.Info("Starting Pod controller")

	go c.informer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	go wait.UntilWithContext(ctx, c.runWorker, time.Second)

	select {
	case <-ctx.Done():
		log.Info("Stopping Pod controller")
		return
	}
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem() {
	}
}

type flags struct {
	Kubeconfig string
	Namespace  string
	Timeout    string
	Selector   string
}

var cliFlags flags

// var podsGetter v1.PodsGetter
// var logOptions v1.PodLogOptions
var podsGetter typedv1.PodInterface
var logOptions *v1.PodLogOptions = &v1.PodLogOptions{}

// var selector labels.Selector
var selector = func(options *metav1.ListOptions) {
	labelSelector, _ := labels.Parse(cliFlags.Selector)
	options.LabelSelector = labelSelector.String()
}

var rootCmd = &cobra.Command{
	Use:  "helm-upgrade-pod-logs",
	Long: "Print new created pod logs if container goes in failed state",
	RunE: func(cmd *cobra.Command, args []string) error {
		return execute()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cliFlags.Kubeconfig, "kubeconfig", "", "path to kubeconfig")
	rootCmd.PersistentFlags().StringVar(&cliFlags.Timeout, "timeout", "5m0s", "process timeout")
	rootCmd.PersistentFlags().StringVarP(&cliFlags.Namespace, "namespace", "n", "default", "namespace")
	rootCmd.PersistentFlags().StringVarP(&cliFlags.Selector, "selector", "l", "", "Selector (label query) to filter on")

	if cliFlags.Kubeconfig == "" {
		cliFlags.Kubeconfig = os.Getenv("KUBECONFIG")
	}

	// selector, _ := labels.ParseSelector(cliFlags.Selector)
	// selector, _ = labels.Parse(cliFlags.Selector)
}

// func parseSelectors() map[string]string {
// 	var labels map[string]string
//
// 	for _, selector := range strings.Split(cliFlags.Selector, ",") {
// 		kv := strings.Split(selector, "=")
//
// 		spew.Dump(kv)
// 		// labels[kv[0]] = kv[1]
// 	}
//
// 	spew.Dump(labels)
//
// 	return labels
// }

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Error(err)
	}
}

func execute() error {
	kubeconfig, err := ioutil.ReadFile(cliFlags.Kubeconfig)
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

	podsGetter = clientset.CoreV1().Pods(cliFlags.Namespace)

	// podListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", cliFlags.Namespace, fields.Everything())
	podListWatcher := cache.NewFilteredListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", cliFlags.Namespace, selector)

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

	timeout, err := time.ParseDuration(cliFlags.Timeout)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	controller := NewController(queue, indexer, informer)
	controller.Run(ctx, 1)

	return nil
}
