// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver && test

package kubeapiserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakeclientset "k8s.io/client-go/kubernetes/fake"

	"github.com/DataDog/datadog-agent/comp/core/config"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
)

func TestStoreGenerators(t *testing.T) {
	// Define tests
	tests := []struct {
		name                    string
		cfg                     map[string]interface{}
		expectedStoresGenerator []storeGenerator
	}{
		{
			name: "All configurations disabled",
			cfg: map[string]interface{}{
				"cluster_agent.collect_kubernetes_tags": false,
				"language_detection.reporting.enabled":  false,
				"language_detection.enabled":            false,
			},
			expectedStoresGenerator: []storeGenerator{},
		},
		{
			name: "All configurations disabled",
			cfg: map[string]interface{}{
				"cluster_agent.collect_kubernetes_tags": false,
				"language_detection.reporting.enabled":  false,
				"language_detection.enabled":            true,
			},
			expectedStoresGenerator: []storeGenerator{},
		},
		{
			name: "Kubernetes tags enabled",
			cfg: map[string]interface{}{
				"cluster_agent.collect_kubernetes_tags": true,
				"language_detection.reporting.enabled":  false,
				"language_detection.enabled":            true,
			},
			expectedStoresGenerator: []storeGenerator{newPodStore},
		},
		{
			name: "Language detection enabled",
			cfg: map[string]interface{}{
				"cluster_agent.collect_kubernetes_tags": false,
				"language_detection.reporting.enabled":  true,
				"language_detection.enabled":            true,
			},
			expectedStoresGenerator: []storeGenerator{newDeploymentStore},
		},
		{
			name: "Language detection enabled",
			cfg: map[string]interface{}{
				"cluster_agent.collect_kubernetes_tags": false,
				"language_detection.reporting.enabled":  true,
				"language_detection.enabled":            false,
			},
			expectedStoresGenerator: []storeGenerator{},
		},
		{
			name: "All configurations enabled",
			cfg: map[string]interface{}{
				"cluster_agent.collect_kubernetes_tags": true,
				"language_detection.reporting.enabled":  true,
				"language_detection.enabled":            true,
			},
			expectedStoresGenerator: []storeGenerator{newPodStore, newDeploymentStore},
		},
	}

	// Run test for each testcase
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fxutil.Test[config.Component](t, fx.Options(
				config.MockModule(),
				fx.Replace(config.MockParams{Overrides: tt.cfg}),
			))
			expectedStores := collectResultStoreGenerator(tt.expectedStoresGenerator, cfg)
			stores := collectResultStoreGenerator(storeGenerators(cfg), cfg)

			assert.Equal(t, expectedStores, stores)
		})
	}
}

func collectResultStoreGenerator(funcs []storeGenerator, config config.Reader) []*reflectorStore {
	var stores []*reflectorStore
	for _, f := range funcs {
		_, s := f(nil, nil, config, nil)
		stores = append(stores, s)
	}
	return stores
}

func Test_metadataCollectionGVRs_WithFunctionalDiscovery(t *testing.T) {
	tests := []struct {
		name                  string
		apiServerResourceList []*metav1.APIResourceList
		expectedGVRs          []schema.GroupVersionResource
		cfg                   map[string]interface{}
	}{
		{
			name:                  "no requested resources, no resources at all!",
			apiServerResourceList: []*metav1.APIResourceList{},
			expectedGVRs:          []schema.GroupVersionResource{},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "",
			},
		},
		{
			name:                  "requested resources, but no resources at all!",
			apiServerResourceList: []*metav1.APIResourceList{},
			expectedGVRs:          []schema.GroupVersionResource{},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/deployments",
			},
		},
		{
			name: "only one resource (statefulsets), only one version, correct resource requested",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "Statefulset",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{{Resource: "statefulsets", Group: "apps", Version: "v1"}},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/statefulsets",
			},
		},
		{
			name: "deployments should be skipped from metadata collection",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/deployments",
			},
		},
		{
			name: "pods should be skipped from metadata collection",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "pods",
							Kind:       "Pod",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "/pods",
			},
		},
		{
			name: "only one resource (statefulsets), only one version, correct resource requested, but version is empty (with double slash)",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "Statefulset",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{{Resource: "statefulsets", Group: "apps", Version: "v1"}},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps//statefulsets",
			},
		},
		{
			name:                  "only one resource with specific version",
			apiServerResourceList: []*metav1.APIResourceList{},
			expectedGVRs:          []schema.GroupVersionResource{{Resource: "foo", Group: "g", Version: "v"}},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "g/v/foo",
			},
		},
		{
			name: "only one resource (deployments), only one version, wrong resource requested",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/daemonsets",
			},
		},
		{
			name: "multiple resources (deployments, statefulsets), multiple versions, all resources requested",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "StatefulSet",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "StatefulSet",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{
				{Resource: "statefulsets", Group: "apps", Version: "v1"},
			},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/deployments apps/statefulsets",
			},
		},
		{
			name: "multiple resources (deployments, statefulsets), multiple versions, only one resource requested",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "StatefulSet",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "StatefulSet",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/deployments",
			},
		},
		{
			name: "multiple resources (deployments, statefulsets), multiple versions, two resources requested (one with a typo)",
			apiServerResourceList: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "deployments",
							Kind:       "Deployment",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "daemonsets",
							Kind:       "Daemonset",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "daemonsets",
							Kind:       "Daemonset",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "StatefulSet",
							Namespaced: true,
						},
					},
				},
				{
					GroupVersion: "apps/v1beta1",
					APIResources: []metav1.APIResource{
						{
							Name:       "statefulsets",
							Kind:       "StatefulSet",
							Namespaced: true,
						},
					},
				},
			},
			expectedGVRs: []schema.GroupVersionResource{
				{Resource: "daemonsets", Group: "apps", Version: "v1"},
			},
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/daemonsets apps/statefulsetsy",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(tt *testing.T) {
			cfg := fxutil.Test[config.Component](t, fx.Options(
				config.MockModule(),
				fx.Replace(config.MockParams{Overrides: test.cfg}),
			))

			client := fakeclientset.NewSimpleClientset()
			fakeDiscoveryClient, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
			assert.Truef(tt, ok, "Failed to initialise fake discovery client")

			fakeDiscoveryClient.Resources = test.apiServerResourceList

			discoveredGVRs, err := metadataCollectionGVRs(cfg, fakeDiscoveryClient)
			require.NoErrorf(tt, err, "Function should not have returned an error")

			assert.ElementsMatch(tt, test.expectedGVRs, discoveredGVRs)
		})
	}
}

func TestResourcesWithMetadataCollectionEnabled(t *testing.T) {
	tests := []struct {
		name              string
		cfg               map[string]interface{}
		expectedResources []string
	}{
		{
			name: "no resources requested",
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "",
			},
			expectedResources: []string{"//nodes"},
		},
		{
			name: "duplicate versions for the same group/resource should not be allowed",
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/deployments apps/statefulsets apps//deployments apps/v1/statefulsets apps/v1/daemonsets",
			},
			expectedResources: []string{"//nodes", "apps/v1/daemonsets"},
		},
		{
			name: "with generic resource tagging based on annotations and/or labels configured",
			cfg: map[string]interface{}{
				"language_detection.enabled":                       false,
				"language_detection.reporting.enabled":             false,
				"cluster_agent.kube_metadata_collection.enabled":   false,
				"cluster_agent.kube_metadata_collection.resources": "",
				"kubernetes_resources_labels_as_tags":              `{"deployments.apps": {"x-team": "team"}, "custom.example.com": {"x-team": "team"}}`,
				"kubernetes_resources_annotations_as_tags":         `{"namespaces": {"x-team": "team"}}`,
			},
			expectedResources: []string{"//nodes", "//namespaces", "example.com//custom"},
		},
		{
			name: "generic resources tagging should be exclude invalid resources",
			cfg: map[string]interface{}{
				"language_detection.enabled":                       false,
				"language_detection.reporting.enabled":             false,
				"cluster_agent.kube_metadata_collection.enabled":   false,
				"cluster_agent.kube_metadata_collection.resources": "",
				"kubernetes_resources_labels_as_tags":              `{"-invalid": {"x-team": "team"}, "invalid.exa_mple.com": {"x-team": "team"}}`,
				"kubernetes_resources_annotations_as_tags":         `{"in valid.example.com": {"x-team": "team"}, "invalid.example.com-": {"x-team": "team"}}`,
			},
			expectedResources: []string{"//nodes"},
		},
		{
			name: "deployments should be excluded from metadata collection",
			cfg: map[string]interface{}{
				"language_detection.enabled":                       true,
				"language_detection.reporting.enabled":             true,
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/daemonsets apps/deployments",
			},
			expectedResources: []string{"apps//daemonsets", "//nodes"},
		},
		{
			name: "pods should be excluded from metadata collection",
			cfg: map[string]interface{}{
				"autoscaling.workload.enabled":                     true,
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/daemonsets pods",
			},
			expectedResources: []string{"apps//daemonsets", "//nodes"},
		},
		{
			name: "resources explicitly requested",
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "apps/deployments apps/statefulsets example.com/custom",
			},
			expectedResources: []string{"//nodes", "apps//statefulsets", "example.com//custom"},
		},
		{
			name: "namespaces needed for namespace labels as tags",
			cfg: map[string]interface{}{
				"kubernetes_namespace_labels_as_tags": map[string]string{
					"label1": "tag1",
				},
			},
			expectedResources: []string{"//nodes", "//namespaces"},
		},
		{
			name: "namespaces needed for namespace annotations as tags",
			cfg: map[string]interface{}{
				"kubernetes_namespace_annotations_as_tags": map[string]string{
					"annotation1": "tag1",
				},
			},
			expectedResources: []string{"//nodes", "//namespaces"},
		},
		{
			name: "namespaces needed for namespace labels and annotations as tags",
			cfg: map[string]interface{}{
				"kubernetes_namespace_labels_as_tags":      `{"label1": "tag1"}`,
				"kubernetes_namespace_annotations_as_tags": `{"annotation1": "tag2"}`,
			},
			expectedResources: []string{"//nodes", "//namespaces"},
		},
		{
			name: "resources explicitly requested and also needed for namespace labels as tags",
			cfg: map[string]interface{}{
				"cluster_agent.kube_metadata_collection.enabled":   true,
				"cluster_agent.kube_metadata_collection.resources": "namespaces apps/deployments",
				"kubernetes_namespace_labels_as_tags":              `{"label1": "tag1"}`,
			},
			expectedResources: []string{"//nodes", "//namespaces"}, // namespaces are not duplicated
		},
		{
			name: "resources explicitly requested with apm enabled and also needed for namespace labels as tags",
			cfg: map[string]interface{}{
				"apm_config.instrumentation.enabled":                                     true,
				"apm_config.instrumentation.targets":                                     []interface{}{"target-1"},
				"cluster_agent.kube_metadata_collection.enabled":                         true,
				"cluster_agent.kube_metadata_collection.resources":                       "namespaces apps/deployments",
				"kubernetes_namespace_labels_as_tagkubernetes_namespace_labels_as_tagss": `{"label1": "tag1"}`,
			},
			expectedResources: []string{"//nodes", "//namespaces"}, // namespaces are not duplicated
		},
		{
			name: "apm enabled enables namespace collection",
			cfg: map[string]interface{}{
				"apm_config.instrumentation.enabled": true,
				"apm_config.instrumentation.targets": []interface{}{"target-1"},
			},
			expectedResources: []string{"//nodes", "//namespaces"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := fxutil.Test[config.Component](t, fx.Options(
				config.MockModule(),
				fx.Replace(config.MockParams{Overrides: test.cfg}),
			))

			assert.ElementsMatch(t, test.expectedResources, resourcesWithMetadataCollectionEnabled(cfg))
		})
	}
}
