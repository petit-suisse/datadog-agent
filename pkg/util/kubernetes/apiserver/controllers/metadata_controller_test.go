// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	corev1 "k8s.io/api/core/v1"
	discv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/DataDog/datadog-agent/comp/core/config"
	log "github.com/DataDog/datadog-agent/comp/core/log/def"
	logmock "github.com/DataDog/datadog-agent/comp/core/log/mock"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	workloadmetafxmock "github.com/DataDog/datadog-agent/comp/core/workloadmeta/fx-mock"
	workloadmetamock "github.com/DataDog/datadog-agent/comp/core/workloadmeta/mock"
	apiv1 "github.com/DataDog/datadog-agent/pkg/clusteragent/api/v1"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/apiserver"
	"github.com/DataDog/datadog-agent/pkg/util/testutil"
)

func TestMetadataControllerSyncEndpoints(t *testing.T) {
	client := fake.NewSimpleClientset()

	metaController, informerFactory := newFakeMetadataController(client, newMockWorkloadMeta(t), false)

	// don't use the global store so we can inspect the store without
	// it being modified by other tests.
	metaController.store = &metaBundleStore{
		cache: gocache.New(gocache.NoExpiration, 5*time.Second),
	}

	pod1 := newFakePod(
		"default",
		"pod1_name",
		"1111",
		"1.1.1.1",
	)

	pod2 := newFakePod(
		"default",
		"pod2_name",
		"2222",
		"2.2.2.2",
	)

	pod3 := newFakePod(
		"default",
		"pod3_name",
		"3333",
		"3.3.3.3",
	)

	// Create nodes in workloadmeta
	for _, nodeName := range []string{"node1", "node2", "node3"} {
		err := metaController.wmeta.Push(
			"metadata-controller",
			workloadmeta.Event{
				Type: workloadmeta.EventTypeSet,
				Entity: &workloadmeta.KubernetesMetadata{
					EntityID: workloadmeta.EntityID{
						Kind: workloadmeta.KindKubernetesMetadata,
						ID:   nodeName,
					},
					EntityMeta: workloadmeta.EntityMeta{
						Name: nodeName,
					},
					GVR: &schema.GroupVersionResource{
						Version:  "v1",
						Resource: "nodes",
					},
				},
			},
		)
		require.NoError(t, err)
	}

	// Wait until the workloadmeta events have been processed
	require.Eventually(t, func() bool {
		return len(metaController.wmeta.ListKubernetesMetadata(workloadmeta.IsNodeMetadata)) == 3
	}, 5*time.Second, 100*time.Millisecond)

	tests := []struct {
		desc            string
		delete          bool // whether to add or delete endpoints
		endpoints       *corev1.Endpoints
		expectedBundles map[string]apiv1.NamespacesPodsStringsSet
	}{
		{
			"one service on multiple nodes",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc1"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{
							newFakeEndpointAddress("node1", pod1),
							newFakeEndpointAddress("node2", pod2),
						},
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
			},
		},
		{
			"pod added to service",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc1"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{
							newFakeEndpointAddress("node1", pod1),
							newFakeEndpointAddress("node2", pod2),
							newFakeEndpointAddress("node1", pod3),
						},
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
						"pod3_name": sets.New("svc1"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
			},
		},
		{
			"pod deleted from service",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc1"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{
							newFakeEndpointAddress("node1", pod1),
							newFakeEndpointAddress("node2", pod2),
						},
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
			},
		},
		{
			"pod deleted from service and node clears",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc1"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{
							newFakeEndpointAddress("node1", pod1),
						},
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
					},
				},
			},
		},
		{
			"pod added back to service",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc1"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{
							newFakeEndpointAddress("node1", pod1),
							newFakeEndpointAddress("node2", pod2),
						},
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
			},
		},
		{
			"add service for existing pod",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc2"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{
							newFakeEndpointAddress("node1", pod1),
						},
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1", "svc2"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
			},
		},
		{
			"delete service with pods on multiple nodes",
			true,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc1"},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc2"),
					},
				},
			},
		},
		{
			"add endpoints for leader election",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "leader-election",
					Annotations: map[string]string{
						"control-plane.alpha.kubernetes.io/leader": `{"holderIdentity":"foo"}`,
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{ // no changes to cluster metadata
				"node1": {
					"default": {
						"pod1_name": sets.New("svc2"),
					},
				},
			},
		},
		{
			"update endpoints for leader election",
			false,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "leader-election",
					Annotations: map[string]string{
						"control-plane.alpha.kubernetes.io/leader": `{"holderIdentity":"bar"}`,
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{ // no changes to cluster metadata
				"node1": {
					"default": {
						"pod1_name": sets.New("svc2"),
					},
				},
			},
		},
		{
			"delete every service",
			true,
			&corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "svc2"},
			},
			map[string]apiv1.NamespacesPodsStringsSet{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			store := informerFactory.
				Core().
				V1().
				Endpoints().
				Informer().
				GetStore()

			var err error
			if tt.delete {
				err = store.Delete(tt.endpoints)
			} else {
				err = store.Add(tt.endpoints)
			}
			require.NoError(t, err)

			key, err := cache.MetaNamespaceKeyFunc(tt.endpoints)
			require.NoError(t, err)

			err = metaController.syncEndpoints(key)
			require.NoError(t, err)

			nonNilKeys := metaController.countNonNilKeys()
			assert.Equal(t, len(tt.expectedBundles), nonNilKeys, "Unexpected metaBundles found")

			for nodeName, expectedMapper := range tt.expectedBundles {
				metaBundle, ok := metaController.store.get(nodeName)
				require.True(t, ok, "No meta bundle for %s", nodeName)
				assert.Equal(t, expectedMapper, metaBundle.Services, nodeName)
			}
		})
	}
}

func TestMetadataControllerSyncEndpointSlices(t *testing.T) {
	client := fake.NewSimpleClientset()

	metaController, informerFactory := newFakeMetadataController(client, newMockWorkloadMeta(t), true)

	metaController.store = &metaBundleStore{
		cache: gocache.New(gocache.NoExpiration, 5*time.Second),
	}

	pod1 := newFakePod(
		"default",
		"pod1_name",
		"1111",
		"1.1.1.1",
	)

	pod2 := newFakePod(
		"default",
		"pod2_name",
		"2222",
		"2.2.2.2",
	)

	pod3 := newFakePod(
		"default",
		"pod3_name",
		"3333",
		"3.3.3.3",
	)

	// Create nodes in workloadmeta
	for _, nodeName := range []string{"node1", "node2", "node3"} {
		err := metaController.wmeta.Push(
			"metadata-controller",
			workloadmeta.Event{
				Type: workloadmeta.EventTypeSet,
				Entity: &workloadmeta.KubernetesMetadata{
					EntityID: workloadmeta.EntityID{
						Kind: workloadmeta.KindKubernetesMetadata,
						ID:   nodeName,
					},
					EntityMeta: workloadmeta.EntityMeta{
						Name: nodeName,
					},
					GVR: &schema.GroupVersionResource{
						Version:  "v1",
						Resource: "nodes",
					},
				},
			},
		)
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return len(metaController.wmeta.ListKubernetesMetadata(workloadmeta.IsNodeMetadata)) == 3
	}, 5*time.Second, 100*time.Millisecond)

	tests := []struct {
		desc             string
		delete           bool
		endpointSlices   []*discv1.EndpointSlice
		expectedBundles  map[string]apiv1.NamespacesPodsStringsSet
		expectedCacheLen int
	}{
		{
			"add service with multiple slices",
			false,
			[]*discv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc1-slice1",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc1"},
						ResourceVersion: "v1",
					},
					Endpoints: []discv1.Endpoint{
						newFakeEndpoint("node1", pod1),
						newFakeEndpoint("node2", pod2),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc1-slice2",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc1"},
						ResourceVersion: "v1",
					},
					Endpoints: []discv1.Endpoint{
						newFakeEndpoint("node3", pod3),
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
				"node3": {
					"default": {
						"pod3_name": sets.New("svc1"),
					},
				},
			},
			2,
		},
		{
			"add new service tied to an existing pod",
			false,
			[]*discv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc2-slice1",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc2"},
						ResourceVersion: "v1",
					},
					Endpoints: []discv1.Endpoint{
						newFakeEndpoint("node1", pod1),
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1", "svc2"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1"),
					},
				},
				"node3": {
					"default": {
						"pod3_name": sets.New("svc1"),
					},
				},
			},
			3, // Cache length increases by 1 for svc2-slice1
		},
		{
			"update slice to add pod",
			false,
			[]*discv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc2-slice1",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc2"},
						ResourceVersion: "v2",
					},
					Endpoints: []discv1.Endpoint{
						newFakeEndpoint("node1", pod1),
						newFakeEndpoint("node2", pod2),
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1", "svc2"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1", "svc2"),
					},
				},
				"node3": {
					"default": {
						"pod3_name": sets.New("svc1"),
					},
				},
			},
			3,
		},
		{
			"update slice to delete pod",
			false,
			[]*discv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc2-slice1",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc2"},
						ResourceVersion: "v3",
					},
					Endpoints: []discv1.Endpoint{
						newFakeEndpoint("node2", pod2),
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node1": {
					"default": {
						"pod1_name": sets.New("svc1"),
					},
				},
				"node2": {
					"default": {
						"pod2_name": sets.New("svc1", "svc2"),
					},
				},
				"node3": {
					"default": {
						"pod3_name": sets.New("svc1"),
					},
				},
			},
			3,
		},
		{
			"delete service with multiple slices",
			true,
			[]*discv1.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc1-slice1",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc1"},
						ResourceVersion: "v2",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       "default",
						Name:            "svc1-slice2",
						Labels:          map[string]string{"kubernetes.io/service-name": "svc1"},
						ResourceVersion: "v2",
					},
				},
			},
			map[string]apiv1.NamespacesPodsStringsSet{
				"node2": {
					"default": {
						"pod2_name": sets.New("svc2"),
					},
				},
			},
			1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			store := informerFactory.
				Discovery().
				V1().
				EndpointSlices().
				Informer().
				GetStore()

			var err error
			if tt.delete {
				for _, slice := range tt.endpointSlices {
					err = store.Delete(slice)
					require.NoError(t, err)
				}
			} else {
				for _, slice := range tt.endpointSlices {
					err = store.Add(slice)
					require.NoError(t, err)
				}
			}

			for _, slice := range tt.endpointSlices {
				key, err := cache.MetaNamespaceKeyFunc(slice)
				require.NoError(t, err)

				err = metaController.syncEndpointSlices(key)
				require.NoError(t, err)
			}

			nonNilKeys := metaController.countNonNilKeys()
			assert.Equal(t, len(tt.expectedBundles), nonNilKeys, "Unexpected metaBundles found")

			for nodeName, expectedMapper := range tt.expectedBundles {
				metaBundle, ok := metaController.store.get(nodeName)
				require.True(t, ok, "No meta bundle for %s", nodeName)
				assert.Equal(t, expectedMapper, metaBundle.Services, nodeName)
			}

			cacheLen := len(metaController.sliceServiceCache["default"])

			assert.Equal(t, tt.expectedCacheLen, cacheLen, "Cache length mismatch")
		})
	}
}

func TestMetadataController(t *testing.T) {
	// FIXME: Updating to k8s.io/client-go v0.9+ should allow revert this PR https://github.com/DataDog/datadog-agent/pull/2524
	// that allows a more fine-grain testing on the controller lifecycle (affected by bug https://github.com/kubernetes/kubernetes/pull/66078)
	client := fake.NewSimpleClientset()

	c := client.CoreV1()
	require.NotNil(t, c)

	// Create a Ready Schedulable node
	// As we don't have a controller they don't need to have some heartbeat mechanism
	node := &corev1.Node{
		Spec: corev1.NodeSpec{
			PodCIDR:       "192.168.1.0/24",
			Unschedulable: false,
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{
					Address: "172.31.119.125",
					Type:    "InternalIP",
				},
				{
					Address: "ip-172-31-119-125.eu-west-1.compute.internal",
					Type:    "InternalDNS",
				},
				{
					Address: "ip-172-31-119-125.eu-west-1.compute.internal",
					Type:    "Hostname",
				},
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:    "Ready",
					Status:  "True",
					Reason:  "KubeletReady",
					Message: "kubelet is posting ready status",
				},
			},
		},
	}
	node.Name = "ip-172-31-119-125"
	_, err := c.Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
		},
		Spec: corev1.PodSpec{
			NodeName: node.Name,
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:latest",
				},
			},
		},
	}
	pod.Name = "nginx"
	pod.Labels = map[string]string{"app": "nginx"}
	pendingPod, err := c.Pods("default").Create(context.TODO(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	pendingPod.Status = corev1.PodStatus{
		Phase:  "Running",
		PodIP:  "172.17.0.1",
		HostIP: "172.31.119.125",
		Conditions: []corev1.PodCondition{
			{
				Type:   "Ready",
				Status: "True",
			},
		},
		// mark it ready
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name:  "nginx",
				Ready: true,
				Image: "nginx:latest",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			},
		},
	}
	_, err = c.Pods("default").UpdateStatus(context.TODO(), pendingPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "nginx",
			},
			Ports: []corev1.ServicePort{{Port: 443}},
		},
	}
	svc.Name = "nginx-1"
	_, err = c.Services("default").Create(context.TODO(), svc, metav1.CreateOptions{})
	require.NoError(t, err)

	ep := &corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP:       pendingPod.Status.PodIP,
						NodeName: &node.Name,
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: pendingPod.Namespace,
							Name:      pendingPod.Name,
							UID:       pendingPod.UID,
						},
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "https",
						Port:     443,
						Protocol: "TCP",
					},
				},
			},
		},
	}
	ep.Name = "nginx-1"
	_, err = c.Endpoints("default").Create(context.TODO(), ep, metav1.CreateOptions{})
	require.NoError(t, err)

	// Add a new service/endpoint on the nginx Pod
	svc.Name = "nginx-2"
	_, err = c.Services("default").Create(context.TODO(), svc, metav1.CreateOptions{})
	require.NoError(t, err)

	ep.Name = "nginx-2"
	_, err = c.Endpoints("default").Create(context.TODO(), ep, metav1.CreateOptions{})
	require.NoError(t, err)

	metaController, informerFactory := newFakeMetadataController(client, newMockWorkloadMeta(t), false)

	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	go metaController.run(stop)

	testutil.AssertTrueBeforeTimeout(t, 100*time.Millisecond, 2*time.Second, func() bool {
		return metaController.listerSynced()
	})

	testutil.AssertTrueBeforeTimeout(t, 100*time.Millisecond, 2*time.Second, func() bool {
		metadataNames, err := apiserver.GetPodMetadataNames(node.Name, pod.Namespace, pod.Name)
		if err != nil {
			return false
		}
		if len(metadataNames) != 2 {
			return false
		}
		assert.Contains(t, metadataNames, "kube_service:nginx-1")
		assert.Contains(t, metadataNames, "kube_service:nginx-2")
		return true
	})

	cl := &apiserver.APIClient{Cl: client}

	testutil.AssertTrueBeforeTimeout(t, 100*time.Millisecond, 2*time.Second, func() bool {
		fullmapper, errList := apiserver.GetMetadataMapBundleOnAllNodes(cl)
		require.Nil(t, errList)
		list := fullmapper.Nodes
		assert.Contains(t, list, "ip-172-31-119-125")
		bundle := apiserver.MetadataMapperBundle{Services: list["ip-172-31-119-125"].Services}
		services, found := bundle.ServicesForPod(metav1.NamespaceDefault, "nginx")
		if !found {
			return false
		}
		assert.Contains(t, services, "nginx-1")
		return true
	})
}

func newMockWorkloadMeta(t *testing.T) workloadmeta.Component {
	return fxutil.Test[workloadmetamock.Mock](
		t,
		fx.Options(
			fx.Provide(func() log.Component { return logmock.New(t) }),
			config.MockModule(),
			workloadmetafxmock.MockModule(workloadmeta.NewParams()),
		),
	)
}

func newFakeMetadataController(client kubernetes.Interface, wmeta workloadmeta.Component, useEndpointSlices bool) (*metadataController, informers.SharedInformerFactory) {
	informerFactory := informers.NewSharedInformerFactory(client, 1*time.Second)

	metaController := newMetadataController(
		informerFactory,
		wmeta,
		useEndpointSlices,
	)

	return metaController, informerFactory
}

func newFakePod(namespace, name, uid, ip string) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uid),
		},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func newFakeEndpointAddress(nodeName string, pod corev1.Pod) corev1.EndpointAddress {
	return corev1.EndpointAddress{
		IP:       pod.Status.PodIP,
		NodeName: &nodeName,
		TargetRef: &corev1.ObjectReference{
			Kind:      pod.Kind,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       pod.UID,
		},
	}
}

func newFakeEndpoint(nodeName string, pod corev1.Pod) discv1.Endpoint {
	return discv1.Endpoint{
		Addresses: []string{pod.Status.PodIP},
		NodeName:  &nodeName,
		TargetRef: &corev1.ObjectReference{
			Kind:      pod.Kind,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       pod.UID,
		},
	}
}

func (m *metadataController) countNonNilKeys() int {
	nonNilKeys := 0
	for _, key := range m.store.listKeys() {
		value, _ := m.store.get(key)
		if value != nil && len(value.Services) > 0 {
			nonNilKeys++
		}
	}
	return nonNilKeys
}

func (m *metaBundleStore) listKeys() []string {
	keys := []string{}
	for k := range m.cache.Items() {
		k = strings.TrimPrefix(k, "agent/KubernetesMetadataMapping/")
		keys = append(keys, k)
	}
	return keys
}
