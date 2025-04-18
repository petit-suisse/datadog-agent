// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024-present Datadog, Inc.

//go:build linux && nvml

// Package testutil holds different utilities and stubs for testing
package testutil

import (
	"fmt"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	nvmlmock "github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/comp/core"
	"github.com/DataDog/datadog-agent/comp/core/telemetry"
	"github.com/DataDog/datadog-agent/comp/core/telemetry/telemetryimpl"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	workloadmetafxmock "github.com/DataDog/datadog-agent/comp/core/workloadmeta/fx-mock"
	workloadmetamock "github.com/DataDog/datadog-agent/comp/core/workloadmeta/mock"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
)

// DefaultGpuCores is the default number of cores for a GPU device in the mock.
const DefaultGpuCores = 10

// GPUUUIDs is a list of UUIDs for the devices returned by the mock
var GPUUUIDs = []string{
	"GPU-00000000-1234-1234-1234-123456789012",
	"GPU-11111111-1234-1234-1234-123456789013",
	"GPU-22222222-1234-1234-1234-123456789014",
	"GPU-33333333-1234-1234-1234-123456789015",
	"GPU-44444444-1234-1234-1234-123456789016",
	"GPU-55555555-1234-1234-1234-123456789017",
	"GPU-66666666-1234-1234-1234-123456789018",
}

// GPUCores is a list of number of cores for the devices returned by the mock,
// should be the same length as GPUUUIDs. If not, GetBasicNvmlMock will panic.
var GPUCores = []int{DefaultGpuCores, 20, 30, 40, 50, 60, 70}

// DefaultGpuUUID is the UUID for the default device returned by the mock
var DefaultGpuUUID = GPUUUIDs[0]

// DefaultGPUName is the name for the default device returned by the mock
var DefaultGPUName = "Tesla T4"

// DefaultNvidiaDriverVersion is the default nvidia driver version
var DefaultNvidiaDriverVersion = "470.57.02"

// DefaultMemoryBusWidth is the memory bus width for the default device returned by the mock
var DefaultMemoryBusWidth = uint32(256)

// DefaultGPUComputeCapMajor is the major number for the compute capabilities for the default device returned by the mock
var DefaultGPUComputeCapMajor = 7

// DefaultGPUComputeCapMinor is the minor number for the compute capabilities for the default device returned by the mock
var DefaultGPUComputeCapMinor = 5

// DefaultGPUArch is the architecture for the default device returned by the mock
var DefaultGPUArch = nvml.DeviceArchitecture(nvml.DEVICE_ARCH_HOPPER)

// DefaultGPUAttributes is the attributes for the default device returned by the mock
var DefaultGPUAttributes = nvml.DeviceAttributes{
	MultiprocessorCount: 10,
}

// DefaultProcessInfo is the list of processes running on the default device returned by the mock
var DefaultProcessInfo = []nvml.ProcessInfo{
	{Pid: 1, UsedGpuMemory: 100},
	{Pid: 5678, UsedGpuMemory: 200},
}

// DefaultTotalMemory is the total memory for the default device returned by the mock
var DefaultTotalMemory = uint64(1000)

// DefaultMaxClockRates is an array of Max SM clock and Max Mem Clock rates for the default device
var DefaultMaxClockRates = [2]uint32{1000, 2000}

// GetDeviceMock returns a mock of the nvml.Device with the given UUID.
func GetDeviceMock(deviceIdx int) *nvmlmock.Device {
	return &nvmlmock.Device{
		GetNumGpuCoresFunc: func() (int, nvml.Return) {
			return GPUCores[deviceIdx], nvml.SUCCESS
		},
		GetCudaComputeCapabilityFunc: func() (int, int, nvml.Return) {
			return 7, 5, nvml.SUCCESS
		},
		GetUUIDFunc: func() (string, nvml.Return) {
			return GPUUUIDs[deviceIdx], nvml.SUCCESS
		},
		GetNameFunc: func() (string, nvml.Return) {
			return DefaultGPUName, nvml.SUCCESS
		},
		GetArchitectureFunc: func() (nvml.DeviceArchitecture, nvml.Return) {
			return DefaultGPUArch, nvml.SUCCESS
		},
		GetAttributesFunc: func() (nvml.DeviceAttributes, nvml.Return) {
			return DefaultGPUAttributes, nvml.SUCCESS
		},
		GetComputeRunningProcessesFunc: func() ([]nvml.ProcessInfo, nvml.Return) {
			return DefaultProcessInfo, nvml.SUCCESS
		},
		GetMemoryInfoFunc: func() (nvml.Memory, nvml.Return) {
			return nvml.Memory{Total: DefaultTotalMemory, Free: 500}, nvml.SUCCESS
		},
		GetMemoryBusWidthFunc: func() (uint32, nvml.Return) {
			return DefaultMemoryBusWidth, nvml.SUCCESS
		},
		GetMaxClockInfoFunc: func(clockType nvml.ClockType) (uint32, nvml.Return) {
			switch clockType {
			case nvml.CLOCK_SM:
				return DefaultMaxClockRates[0], nvml.SUCCESS
			case nvml.CLOCK_MEM:
				return DefaultMaxClockRates[1], nvml.SUCCESS
			default:
				return 0, nvml.ERROR_NOT_SUPPORTED
			}
		},
	}
}

// GetBasicNvmlMock returns a mock of the nvml.Interface with a single device with 10 cores,
// useful for basic tests that need only the basic interaction with NVML to be working.
func GetBasicNvmlMock() *nvmlmock.Interface {
	if len(GPUUUIDs) != len(GPUCores) {
		// Make it really easy to spot errors if we change any of the arrays.
		panic("GPUUUIDs and GPUCores must have the same length, please fix it")
	}

	return &nvmlmock.Interface{
		DeviceGetCountFunc: func() (int, nvml.Return) {
			return len(GPUUUIDs), nvml.SUCCESS
		},
		DeviceGetHandleByIndexFunc: func(index int) (nvml.Device, nvml.Return) {
			return GetDeviceMock(index), nvml.SUCCESS
		},
		DeviceGetCudaComputeCapabilityFunc: func(nvml.Device) (int, int, nvml.Return) {
			return 7, 5, nvml.SUCCESS
		},
		DeviceGetIndexFunc: func(nvml.Device) (int, nvml.Return) {
			return 0, nvml.SUCCESS
		},
		DeviceGetMigModeFunc: func(nvml.Device) (int, int, nvml.Return) {
			return nvml.DEVICE_MIG_DISABLE, 0, nvml.SUCCESS
		},
		DeviceGetComputeRunningProcessesFunc: func(nvml.Device) ([]nvml.ProcessInfo, nvml.Return) {
			return DefaultProcessInfo, nvml.SUCCESS
		},
		DeviceGetMemoryInfoFunc: func(nvml.Device) (nvml.Memory, nvml.Return) {
			return nvml.Memory{Total: DefaultTotalMemory, Free: 500}, nvml.SUCCESS
		},
		SystemGetDriverVersionFunc: func() (string, nvml.Return) {
			return DefaultNvidiaDriverVersion, nvml.SUCCESS
		},
	}
}

// GetWorkloadMetaMock returns a mock of the workloadmeta.Component.
func GetWorkloadMetaMock(t testing.TB) workloadmetamock.Mock {
	return fxutil.Test[workloadmetamock.Mock](t, fx.Options(
		core.MockBundle(),
		workloadmetafxmock.MockModule(workloadmeta.NewParams()),
	))
}

// GetTelemetryMock returns a mock of the telemetry.Component.
func GetTelemetryMock(t testing.TB) telemetry.Component {
	return fxutil.Test[telemetry.Component](t, telemetryimpl.MockModule())
}

// RequireDevicesEqual checks that the two devices are equal by comparing their UUIDs, which gives a better
// output than using require.Equal on the devices themselves
func RequireDevicesEqual(t *testing.T, expected, actual nvml.Device, msgAndArgs ...interface{}) {
	extraFmt := ""
	if len(msgAndArgs) > 0 {
		extraFmt = fmt.Sprintf(msgAndArgs[0].(string), msgAndArgs[1:]...) + ": "
	}

	expectedUUID, ret := expected.GetUUID()
	require.Equal(t, ret, nvml.SUCCESS, "%s%scannot retrieve UUID for expected device %v%s", extraFmt, expected)

	actualUUID, ret := actual.GetUUID()
	require.Equal(t, ret, nvml.SUCCESS, "%scannot retrieve UUID for actual device %v%s", extraFmt, actual)

	require.Equal(t, expectedUUID, actualUUID, "%sUUIDs do not match", extraFmt)
}

// RequireDeviceListsEqual checks that the two device lists are equal by comparing their UUIDs, which gives a better
// output than using require.ElementsMatch on the lists themselves
func RequireDeviceListsEqual(t *testing.T, expected, actual []nvml.Device, msgAndArgs ...interface{}) {
	extraFmt := ""
	if len(msgAndArgs) > 0 {
		extraFmt = fmt.Sprintf(msgAndArgs[0].(string), msgAndArgs[1:]...) + ": "
	}

	require.Len(t, actual, len(expected), "%sdevice lists have different lengths", extraFmt)

	for i := range expected {
		expectedUUID, ret := expected[i].GetUUID()
		require.Equal(t, ret, nvml.SUCCESS, "%scannot retrieve UUID for expected device index %d", extraFmt, i)

		actualUUID, ret := actual[i].GetUUID()
		require.Equal(t, ret, nvml.SUCCESS, "%scannot retrieve UUID for actual device index %d", extraFmt, i)

		require.Equal(t, expectedUUID, actualUUID, "%sUUIDs do not match for element %d", extraFmt, i)
	}
}
