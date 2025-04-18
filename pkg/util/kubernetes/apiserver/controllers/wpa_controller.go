// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package controllers

import (
	"context"
	"fmt"
	"maps"
	"math"
	"time"

	apis_v1alpha1 "github.com/DataDog/watermarkpodautoscaler/apis/datadoghq/v1alpha1"
	"github.com/cenkalti/backoff"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamic_client "k8s.io/client-go/dynamic"
	dynamic_informer "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/DataDog/datadog-agent/pkg/clusteragent/autoscaling/custommetrics"
	"github.com/DataDog/datadog-agent/pkg/errors"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/autoscalers"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	crdCheckInitialInterval = time.Second * 5
	crdCheckMaxInterval     = 5 * time.Minute
	crdCheckMultiplier      = 2.0
	crdCheckMaxElapsedTime  = 0
)

var gvrWPA = apis_v1alpha1.GroupVersion.WithResource("watermarkpodautoscalers")

// runWPA starts the controller to process events about Watermark Pod Autoscalers
func (h *autoscalersController) runWPA(stopCh <-chan struct{}, wpaClient dynamic_client.Interface, wpaInformerFactory dynamic_informer.DynamicSharedInformerFactory) {
	waitForWPACRD(wpaClient)

	// mutate the Autoscaler controller to embed an informer against the WPAs
	if err := h.enableWPA(wpaInformerFactory); err != nil {
		log.Errorf("impossible to enable WPQ: %v", err)
		return
	}

	defer h.wpaQueue.ShutDown()

	log.Infof("Starting WPA Controller ... ")
	defer log.Infof("Stopping WPA Controller")

	wpaInformerFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, h.wpaListerSynced) {
		return
	}

	wait.Until(h.workerWPA, time.Second, stopCh)
}

type checkAPI func() error

func tryCheckWPACRD(check checkAPI) error {
	if err := check(); err != nil {
		// Check if this is a known problem of missing CRD registration
		if isWPACRDNotFoundError(err) {
			return err
		}
		// In all other cases return a permanent error to prevent from retrying
		log.Errorf("WPA CRD check failed: not retryable: %s", err)
		return backoff.Permanent(err)
	}
	log.Info("WPA CRD check successful")
	return nil
}

func notifyCheckWPACRD() backoff.Notify {
	attempt := 0
	return func(_ error, delay time.Duration) {
		attempt++
		mins := int(delay.Minutes())
		secs := int(math.Mod(delay.Seconds(), 60))
		log.Warnf("WPA CRD missing (attempt=%d): will retry in %dm%ds", attempt, mins, secs)
	}
}

func isWPACRDNotFoundError(err error) bool {
	status, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	reason := status.Status().Reason
	details := status.Status().Details
	return reason == v1.StatusReasonNotFound &&
		details.Group == apis_v1alpha1.SchemeGroupVersion.Group &&
		details.Kind == "watermarkpodautoscalers"
}

func checkWPACRD(wpaClient dynamic_client.Interface) backoff.Operation {
	check := func() error {
		_, err := wpaClient.Resource(gvrWPA).List(context.TODO(), v1.ListOptions{})
		return err
	}
	return func() error {
		return tryCheckWPACRD(check)
	}
}

func waitForWPACRD(wpaClient dynamic_client.Interface) {
	exp := &backoff.ExponentialBackOff{
		InitialInterval:     crdCheckInitialInterval,
		RandomizationFactor: 0,
		Multiplier:          crdCheckMultiplier,
		MaxInterval:         crdCheckMaxInterval,
		MaxElapsedTime:      crdCheckMaxElapsedTime,
		Clock:               backoff.SystemClock,
	}
	exp.Reset()
	_ = backoff.RetryNotify(checkWPACRD(wpaClient), exp, notifyCheckWPACRD())
}

// enableWPA adds the handlers to the autoscalersController to support WPAs
func (h *autoscalersController) enableWPA(wpaInformerFactory dynamic_informer.DynamicSharedInformerFactory) error {
	log.Info("Enabling WPA controller")

	genericInformer := wpaInformerFactory.ForResource(gvrWPA)

	h.wpaQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedItemBasedRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "wpa-autoscalers"},
	)
	h.wpaLister = genericInformer.Lister()
	h.wpaListerSynced = genericInformer.Informer().HasSynced
	if _, err := genericInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    h.addWPAutoscaler,
			UpdateFunc: h.updateWPAutoscaler,
			DeleteFunc: h.deleteWPAutoscaler,
		},
	); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wpaEnabled = true
	return nil
}

func (h *autoscalersController) isWPAEnabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wpaEnabled
}

func (h *autoscalersController) workerWPA() {
	for h.processNextWPA() {
	}
}

func (h *autoscalersController) processNextWPA() bool {
	key, quit := h.wpaQueue.Get()
	if quit {
		log.Error("WPA controller hpaQueue is shutting down, stopping processing")
		return false
	}
	log.Tracef("Processing %s", key)
	defer h.wpaQueue.Done(key)

	err := h.syncWPA(key)
	h.handleErr(err, key)

	// Debug output for unit tests only
	if h.autoscalers != nil {
		h.autoscalers <- key
	}
	return true
}

func (h *autoscalersController) syncWPA(key interface{}) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	ns, name, err := cache.SplitMetaNamespaceKey(key.(string))
	if err != nil {
		log.Errorf("Could not split the key: %v", err)
		return err
	}

	wpaCachedObj, err := h.wpaLister.ByNamespace(ns).Get(name)
	if err != nil {
		log.Errorf("Could not retrieve key %s from cache: %v", key, err)
		return err
	}
	wpaCached := &apis_v1alpha1.WatermarkPodAutoscaler{}
	err = UnstructuredIntoWPA(wpaCachedObj, wpaCached)
	if err != nil {
		log.Errorf("Could not cast wpa %s retrieved from cache to wpa structure: %v", key, err)
		return err
	}
	switch {
	case errors.IsNotFound(err):
		log.Infof("WatermarkPodAutoscaler %v has been deleted but was not caught in the EventHandler. GC will cleanup.", key)
	case err != nil:
		log.Errorf("Unable to retrieve WatermarkPodAutoscaler %v from store: %v", key, err)
	default:
		if wpaCached == nil {
			log.Errorf("Could not parse empty wpa %s/%s from local store", ns, name)
			return errIsEmpty
		}
		emList := autoscalers.InspectWPA(wpaCached)
		if len(emList) == 0 {
			return nil
		}
		newMetrics := h.hpaProc.ProcessEMList(emList)
		h.toStore.m.Lock()
		maps.Copy(h.toStore.data, newMetrics)
		h.toStore.m.Unlock()

		log.Tracef("Local batch cache of WPA is %v", h.toStore.data)
	}

	return err
}

func (h *autoscalersController) addWPAutoscaler(obj interface{}) {
	newAutoscaler := &apis_v1alpha1.WatermarkPodAutoscaler{}
	if err := UnstructuredIntoWPA(obj, newAutoscaler); err != nil {
		log.Errorf("Unable to cast obj %s to a WPA: %v", obj, err)
		return
	}
	log.Debugf("Adding WPA %s/%s", newAutoscaler.Namespace, newAutoscaler.Name)
	h.eventRecorder.Event(newAutoscaler.DeepCopyObject(), corev1.EventTypeNormal, autoscalerNowHandleMsgEvent, "")
	h.enqueueWPA(newAutoscaler)
}

//nolint:revive // TODO(CAPP) Fix revive linter
func (h *autoscalersController) updateWPAutoscaler(old, obj interface{}) {
	newAutoscaler := &apis_v1alpha1.WatermarkPodAutoscaler{}
	if err := UnstructuredIntoWPA(obj, newAutoscaler); err != nil {
		log.Errorf("Unable to cast obj %s to a WPA: %v", obj, err)
		return
	}
	oldAutoscaler := &apis_v1alpha1.WatermarkPodAutoscaler{}
	if err := UnstructuredIntoWPA(obj, oldAutoscaler); err != nil {
		log.Errorf("Unable to cast obj %s to a WPA: %v", obj, err)
		h.enqueueWPA(newAutoscaler) // We still want to enqueue the newAutoscaler to get the new change
		return
	}

	if !autoscalers.AutoscalerMetricsUpdate(newAutoscaler.GetObjectMeta(), oldAutoscaler.GetObjectMeta()) {
		log.Tracef("Update received for the %s/%s, without a relevant change to the configuration", newAutoscaler.Namespace, newAutoscaler.Name)
		return
	}
	// Need to delete the old object from the local cache. If the labels have changed, the syncAutoscaler would not override the old key.
	toDelete := autoscalers.InspectWPA(oldAutoscaler)
	h.deleteFromLocalStore(toDelete)
	log.Tracef("Processing update event for wpa %s/%s with configuration: %s", newAutoscaler.Namespace, newAutoscaler.Name, newAutoscaler.Annotations)
	h.enqueueWPA(newAutoscaler)
}

// Processing the Delete Events in the Eventhandler as obj is deleted from the local store thereafter.
// Only here can we retrieve the content of the WPA to properly process and delete it.
// FIXME we could have an update in the wpaQueue while processing the deletion, we should make
// sure we process them in order instead. For now, the gc logic allows us to recover.
func (h *autoscalersController) deleteWPAutoscaler(obj interface{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	toDelete := &custommetrics.MetricsBundle{}
	deletedWPA := &apis_v1alpha1.WatermarkPodAutoscaler{}
	if err := UnstructuredIntoWPA(obj, deletedWPA); err == nil {
		toDelete.External = autoscalers.InspectWPA(deletedWPA)
		h.deleteFromLocalStore(toDelete.External)
		log.Debugf("Deleting %s/%s from the local cache", deletedWPA.Namespace, deletedWPA.Name)
		if !h.isLeaderFunc() {
			return
		}
		log.Infof("Deleting entries of metrics from Ref %s/%s in the Global Store", deletedWPA.Namespace, deletedWPA.Name)
		if err := h.store.DeleteExternalMetricValues(toDelete); err != nil {
			h.enqueueWPA(deletedWPA)
		}
		return
	}

	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		log.Errorf("Could not get object from tombstone %#v", obj)
		return
	}
	if err := UnstructuredIntoWPA(tombstone, deletedWPA); err != nil {
		log.Errorf("Tombstone contained object that is not an Autoscaler: %#v", obj)
		return
	}
	log.Debugf("Deleting Metrics from WPA %s/%s", deletedWPA.Namespace, deletedWPA.Name)
	toDelete.External = autoscalers.InspectWPA(deletedWPA)
	log.Debugf("Deleting %s/%s from the local cache", deletedWPA.Namespace, deletedWPA.Name)
	h.deleteFromLocalStore(toDelete.External)
	if err := h.store.DeleteExternalMetricValues(toDelete); err != nil {
		h.enqueueWPA(deletedWPA)
		return
	}
}

// UnstructuredIntoWPA converts an unstructured into a WPA
func UnstructuredIntoWPA(obj interface{}, structDest *apis_v1alpha1.WatermarkPodAutoscaler) error {
	unstrObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("could not cast Unstructured object: %v", obj)
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(unstrObj.UnstructuredContent(), structDest)
}
