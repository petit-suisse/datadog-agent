// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package workloadmeta

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/pkg/util/pointer"
)

func container1(testTime time.Time) Container {
	return Container{
		EntityID: EntityID{
			Kind: KindContainer,
			ID:   "foo1",
		},
		EntityMeta: EntityMeta{
			Name:      "foo1-name",
			Namespace: "",
		},
		Ports: []ContainerPort{
			{
				Name:     "port1",
				Port:     42000,
				Protocol: "tcp",
			},
			{
				Port:     42001,
				Protocol: "udp",
			},
			{
				Port: 42002,
			},
			{
				Port: 42004,
			},
		},
		State: ContainerState{
			Running:    true,
			CreatedAt:  testTime,
			StartedAt:  testTime,
			FinishedAt: time.Time{},
			Health:     ContainerHealthUnknown,
		},
		CollectorTags: []string{"tag1", "tag2"},
		EnvVars: map[string]string{
			"DD_SERVICE-partial": "my-svc",
		},
	}
}

func container2() Container {
	return Container{
		EntityID: EntityID{
			Kind: KindContainer,
			ID:   "foo1",
		},
		EntityMeta: EntityMeta{
			Name:      "foo1-name",
			Namespace: "",
		},
		Ports: []ContainerPort{
			{
				Port:     42000,
				Protocol: "tcp",
			},
			{
				Port:     42001,
				Protocol: "udp",
			},
			{
				Port:     42002,
				Protocol: "tcp",
			},
			{
				Port: 42003,
			},
			{
				Port:     42004,
				HostPort: 42004,
			},
		},
		State: ContainerState{
			CreatedAt:  time.Time{},
			StartedAt:  time.Time{},
			FinishedAt: time.Time{},
			ExitCode:   pointer.Ptr(int64(100)),
			Health:     ContainerHealthHealthy,
		},
		CollectorTags: []string{"tag3"},
		EnvVars: map[string]string{
			"DD_SERVICE-partial": "my-svc",
			"DD_ENV-extra":       "prod",
		},
	}
}

func TestMerge(t *testing.T) {
	testTime := time.Now()

	expectedContainer := Container{
		EntityID: EntityID{
			Kind: KindContainer,
			ID:   "foo1",
		},
		EntityMeta: EntityMeta{
			Name:      "foo1-name",
			Namespace: "",
		},
		State: ContainerState{
			Running:    true,
			CreatedAt:  testTime,
			StartedAt:  testTime,
			FinishedAt: time.Time{},
			ExitCode:   pointer.Ptr(int64(100)),
			Health:     ContainerHealthHealthy,
		},
		EnvVars: map[string]string{
			"DD_SERVICE-partial": "my-svc",
			"DD_ENV-extra":       "prod",
		},
	}

	expectedPorts := []ContainerPort{
		{
			Name:     "port1",
			Port:     42000,
			Protocol: "tcp",
		},
		{
			Port:     42001,
			Protocol: "udp",
		},
		{
			Port: 42002,
		},
		{
			Port:     42002,
			Protocol: "tcp",
		},
		{
			Port: 42003,
		},
		{
			Port:     42004,
			HostPort: 42004,
		},
	}

	expectedTags := []string{"tag1", "tag2", "tag3"}

	// Test merging both ways
	fromSource1 := container1(testTime)
	fromSource2 := container2()
	err := merge(&fromSource1, &fromSource2)
	assert.NoError(t, err)
	assert.ElementsMatch(t, expectedPorts, fromSource1.Ports)
	assert.ElementsMatch(t, expectedTags, fromSource1.CollectorTags)
	fromSource1.Ports = nil
	fromSource1.CollectorTags = nil
	assert.Equal(t, expectedContainer, fromSource1)

	fromSource1 = container1(testTime)
	fromSource2 = container2()
	err = merge(&fromSource2, &fromSource1)
	assert.NoError(t, err)
	assert.ElementsMatch(t, expectedPorts, fromSource2.Ports)
	assert.ElementsMatch(t, expectedTags, fromSource2.CollectorTags)
	fromSource2.Ports = nil
	fromSource2.CollectorTags = nil
	assert.Equal(t, expectedContainer, fromSource2)

	// Test merging nil slice in src/dst
	fromSource1 = container1(testTime)
	fromSource2 = container2()
	fromSource2.Ports = nil
	err = merge(&fromSource1, &fromSource2)
	assert.NoError(t, err)
	assert.ElementsMatch(t, container1(testTime).Ports, fromSource1.Ports)

	fromSource1 = container1(testTime)
	fromSource2 = container2()
	fromSource2.Ports = nil
	err = merge(&fromSource2, &fromSource1)
	assert.NoError(t, err)
	assert.ElementsMatch(t, container1(testTime).Ports, fromSource2.Ports)
}
