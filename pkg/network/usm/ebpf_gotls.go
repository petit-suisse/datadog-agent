// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf

package usm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	manager "github.com/DataDog/ebpf-manager"

	ddebpf "github.com/DataDog/datadog-agent/pkg/ebpf"
	"github.com/DataDog/datadog-agent/pkg/network/config"
	"github.com/DataDog/datadog-agent/pkg/network/go/bininspect"
	"github.com/DataDog/datadog-agent/pkg/network/protocols"
	"github.com/DataDog/datadog-agent/pkg/network/protocols/http/gotls"
	"github.com/DataDog/datadog-agent/pkg/network/protocols/http/gotls/lookup"
	libtelemetry "github.com/DataDog/datadog-agent/pkg/network/protocols/telemetry"
	"github.com/DataDog/datadog-agent/pkg/network/usm/buildmode"
	usmconfig "github.com/DataDog/datadog-agent/pkg/network/usm/config"
	"github.com/DataDog/datadog-agent/pkg/network/usm/consts"
	"github.com/DataDog/datadog-agent/pkg/network/usm/utils"
	"github.com/DataDog/datadog-agent/pkg/process/monitor"
	"github.com/DataDog/datadog-agent/pkg/util/kernel"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/datadog-agent/pkg/util/safeelf"
)

const (
	offsetsDataMap            = "offsets_data"
	goTLSReadArgsMap          = "go_tls_read_args"
	goTLSWriteArgsMap         = "go_tls_write_args"
	connectionTupleByGoTLSMap = "conn_tup_by_go_tls_conn"

	// The interval of the periodic scan for terminated processes. Increasing the interval, might cause larger spikes in cpu
	// and lowering it might cause constant cpu usage.
	scanTerminatedProcessesInterval = 30 * time.Second

	connReadProbe     = "uprobe__crypto_tls_Conn_Read"
	connReadRetProbe  = "uprobe__crypto_tls_Conn_Read__return"
	connWriteProbe    = "uprobe__crypto_tls_Conn_Write"
	connWriteRetProbe = "uprobe__crypto_tls_Conn_Write__return"
	connCloseProbe    = "uprobe__crypto_tls_Conn_Close"

	// GoTLSAttacherName holds the name used for the uprobe attacher of go-tls programs. Used for tests.
	GoTLSAttacherName = "go-tls"
)

type uprobesInfo struct {
	functionInfo string
	returnInfo   string
}

var functionToProbes = map[string]uprobesInfo{
	bininspect.ReadGoTLSFunc: {
		functionInfo: connReadProbe,
		returnInfo:   connReadRetProbe,
	},
	bininspect.WriteGoTLSFunc: {
		functionInfo: connWriteProbe,
		returnInfo:   connWriteRetProbe,
	},
	bininspect.CloseGoTLSFunc: {
		functionInfo: connCloseProbe,
	},
}

var functionsConfig = map[string]bininspect.FunctionConfiguration{
	bininspect.WriteGoTLSFunc: {
		IncludeReturnLocations: true,
		ParamLookupFunction:    lookup.GetWriteParams,
	},
	bininspect.ReadGoTLSFunc: {
		IncludeReturnLocations: true,
		ParamLookupFunction:    lookup.GetReadParams,
	},
	bininspect.CloseGoTLSFunc: {
		IncludeReturnLocations: false,
		ParamLookupFunction:    lookup.GetCloseParams,
	},
}

var structFieldsLookupFunctions = map[bininspect.FieldIdentifier]bininspect.StructLookupFunction{
	bininspect.StructOffsetTLSConn:     lookup.GetTLSConnInnerConnOffset,
	bininspect.StructOffsetTCPConn:     lookup.GetTCPConnInnerConnOffset,
	bininspect.StructOffsetNetConnFd:   lookup.GetConnFDOffset,
	bininspect.StructOffsetNetFdPfd:    lookup.GetNetFD_PFDOffset,
	bininspect.StructOffsetPollFdSysfd: lookup.GetFD_SysfdOffset,
}

type pid = uint32

// goTLSProgram contains implementation for go-TLS.
type goTLSProgram struct {
	wg      sync.WaitGroup
	done    chan struct{}
	cfg     *config.Config
	manager *manager.Manager

	// Path to the process/container's procfs
	procRoot string

	// eBPF map holding the result of binary analysis, indexed by binaries'
	// inodes.
	offsetsDataMap *ebpf.Map

	// binAnalysisMetric handles telemetry on the time spent doing binary
	// analysis
	binAnalysisMetric *libtelemetry.Counter

	// binNoSymbolsMetric counts Golang binaries without symbols.
	binNoSymbolsMetric *libtelemetry.Counter

	registry *utils.FileRegistry
}

// Validate that goTLSProgram implements the Attacher interface.
var _ utils.Attacher = &goTLSProgram{}

var goTLSSpec = &protocols.ProtocolSpec{
	Factory: newGoTLS,
	Maps: []*manager.Map{
		{Name: offsetsDataMap},
		{Name: goTLSReadArgsMap},
		{Name: goTLSWriteArgsMap},
		{Name: connectionTupleByGoTLSMap},
	},
	Probes: []*manager.Probe{
		{
			ProbeIdentificationPair: manager.ProbeIdentificationPair{
				EBPFFuncName: connReadProbe,
			},
		},
		{
			ProbeIdentificationPair: manager.ProbeIdentificationPair{
				EBPFFuncName: connReadRetProbe,
			},
		},
		{
			ProbeIdentificationPair: manager.ProbeIdentificationPair{
				EBPFFuncName: connWriteProbe,
			},
		},
		{
			ProbeIdentificationPair: manager.ProbeIdentificationPair{
				EBPFFuncName: connWriteRetProbe,
			},
		},
		{
			ProbeIdentificationPair: manager.ProbeIdentificationPair{
				EBPFFuncName: connCloseProbe,
			},
		},
	},
}

func newGoTLS(mgr *manager.Manager, c *config.Config) (protocols.Protocol, error) {
	if !c.EnableGoTLSSupport {
		return nil, nil
	}

	if !usmconfig.TLSSupported(c) {
		log.Warn("goTLS not supported by this platform")
		return nil, nil
	}

	if !c.EnableRuntimeCompiler && !c.EnableCORE {
		log.Warn("goTLS support requires runtime-compilation or CO-RE to be enabled")
		return nil, nil
	}

	return &goTLSProgram{
		done:               make(chan struct{}),
		cfg:                c,
		procRoot:           c.ProcRoot,
		binAnalysisMetric:  libtelemetry.NewCounter("usm.go_tls.analysis_time", libtelemetry.OptPrometheus),
		binNoSymbolsMetric: libtelemetry.NewCounter("usm.go_tls.missing_symbols", libtelemetry.OptPrometheus),
		registry:           utils.NewFileRegistry(consts.USMModuleName, "go-tls"),
		manager:            mgr,
	}, nil
}

// Name return the program's name.
func (p *goTLSProgram) Name() string {
	return "go-tls"
}

// IsBuildModeSupported return true if the build mode is supported.
func (*goTLSProgram) IsBuildModeSupported(mode buildmode.Type) bool {
	return mode == buildmode.CORE || mode == buildmode.RuntimeCompiled
}

// ConfigureOptions changes map attributes to the given options.
func (p *goTLSProgram) ConfigureOptions(options *manager.Options) {
	options.MapSpecEditors[connectionTupleByGoTLSMap] = manager.MapSpecEditor{
		MaxEntries: p.cfg.MaxTrackedConnections,
		EditorFlag: manager.EditMaxEntries,
	}
}

// PreStart launches the goTLS main goroutine to handle events.
func (p *goTLSProgram) PreStart() error {
	var err error

	p.offsetsDataMap, _, err = p.manager.GetMap(offsetsDataMap)
	if err != nil {
		return fmt.Errorf("could not get offsets_data map: %s", err)
	}

	procMonitor := monitor.GetProcessMonitor()
	cleanupExec := procMonitor.SubscribeExec(p.handleProcessStart)
	cleanupExit := procMonitor.SubscribeExit(p.handleProcessExit)

	p.wg.Add(1)
	go func() {
		processSync := time.NewTicker(scanTerminatedProcessesInterval)

		defer func() {
			processSync.Stop()
			cleanupExec()
			cleanupExit()
			procMonitor.Stop()
			p.registry.Clear()
			p.wg.Done()
		}()

		for {
			select {
			case <-p.done:
				return
			case <-processSync.C:
				p.sync()
				p.registry.Log()
			}
		}
	}()

	return nil
}

func (p *goTLSProgram) sync() {
	deletionCandidates := p.registry.GetRegisteredProcesses()

	_ = kernel.WithAllProcs(p.procRoot, func(pid int) error {
		if _, ok := deletionCandidates[uint32(pid)]; ok {
			// We have previously hooked into this process and it remains active,
			// so we remove it from the deletionCandidates list, and move on to the next PID
			delete(deletionCandidates, uint32(pid))
			return nil
		}

		// This is a new PID so we attempt to attach SSL probes to it
		_ = p.AttachPID(uint32(pid))
		return nil
	})

	// At this point all entries from deletionCandidates are no longer alive, so
	// we should detach our SSL probes from them
	for pid := range deletionCandidates {
		p.handleProcessExit(pid)
	}
}

// PostStart registers the goTLS program to the attacher list.
func (p *goTLSProgram) PostStart() error {
	utils.AddAttacher(consts.USMModuleName, p.Name(), p)
	return nil
}

// DumpMaps is a no-op.
func (p *goTLSProgram) DumpMaps(io.Writer, string, *ebpf.Map) {}

// GetStats is a no-op.
func (p *goTLSProgram) GetStats() (*protocols.ProtocolStats, func()) {
	return nil, nil
}

// Stop terminates goTLS main goroutine.
func (p *goTLSProgram) Stop() {
	close(p.done)
	// Waiting for the main event loop to finish.
	p.wg.Wait()
}

var (
	internalProcessRegex = regexp.MustCompile("datadog-agent/.*/((process|security|trace)-agent|system-probe|agent)")
)

// DetachPID detaches the provided PID from the eBPF program.
func (p *goTLSProgram) DetachPID(pid uint32) error {
	return p.registry.Unregister(pid)
}

var (
	// ErrSelfExcluded is returned when the PID is the same as the agent's PID.
	ErrSelfExcluded = errors.New("self-excluded")
	// ErrInternalDDogProcessRejected is returned when the PID is an internal datadog process.
	ErrInternalDDogProcessRejected = errors.New("internal datadog process rejected")
)

// GoTLSAttachPID attaches Go TLS hooks on the binary of process with
// provided PID, if Go TLS is enabled.
func GoTLSAttachPID(pid pid) error {
	if goTLSSpec.Instance == nil {
		return errors.New("GoTLS is not enabled")
	}

	err := goTLSSpec.Instance.(*goTLSProgram).AttachPID(pid)
	if errors.Is(err, utils.ErrPathIsAlreadyRegistered) {
		// The process monitor has attached the process before us.
		return nil
	}

	return err
}

// GoTLSDetachPID detaches Go TLS hooks on the binary of process with
// provided PID, if Go TLS is enabled.
func GoTLSDetachPID(pid pid) error {
	if goTLSSpec.Instance == nil {
		return errors.New("GoTLS is not enabled")
	}

	return goTLSSpec.Instance.(*goTLSProgram).DetachPID(pid)
}

// AttachPID attaches the provided PID to the eBPF program.
func (p *goTLSProgram) AttachPID(pid uint32) error {
	if p.cfg.GoTLSExcludeSelf && pid == uint32(os.Getpid()) {
		return ErrSelfExcluded
	}

	pidAsStr := strconv.FormatUint(uint64(pid), 10)
	exePath := filepath.Join(p.procRoot, pidAsStr, "exe")

	binPath, err := os.Readlink(exePath)
	if err != nil {
		return err
	}

	// Check if the process is datadog's internal process, if so, we don't want to hook the process.
	if internalProcessRegex.MatchString(binPath) {
		if log.ShouldLog(log.DebugLvl) {
			log.Debugf("ignoring pid %d, as it is an internal datadog component (%q)", pid, binPath)
		}
		return ErrInternalDDogProcessRejected
	}

	// Check go process
	probeList := make([]manager.ProbeIdentificationPair, 0)
	return p.registry.Register(binPath, pid, registerCBCreator(p.manager, p.offsetsDataMap, &probeList, p.binAnalysisMetric, p.binNoSymbolsMetric),
		unregisterCBCreator(p.manager, &probeList, p.offsetsDataMap),
		utils.IgnoreCB)
}

func registerCBCreator(mgr *manager.Manager, offsetsDataMap *ebpf.Map, probeIDs *[]manager.ProbeIdentificationPair, binAnalysisMetric, binNoSymbolsMetric *libtelemetry.Counter) func(path utils.FilePath) error {
	return func(filePath utils.FilePath) error {
		start := time.Now()

		f, err := os.Open(filePath.HostPath)
		if err != nil {
			return fmt.Errorf("could not open file %s, %w", filePath.HostPath, err)
		}
		defer f.Close()

		elfFile, err := safeelf.NewFile(f)
		if err != nil {
			return fmt.Errorf("file %s could not be parsed as an ELF file: %w", filePath.HostPath, err)
		}

		inspectionResult, err := bininspect.InspectNewProcessBinary(elfFile, functionsConfig, structFieldsLookupFunctions)
		if err != nil {
			if errors.Is(err, safeelf.ErrNoSymbols) {
				binNoSymbolsMetric.Add(1)
			}
			return fmt.Errorf("error extracting inspection data from %s: %w", filePath.HostPath, err)
		}

		if err := addInspectionResultToMap(offsetsDataMap, filePath.ID, inspectionResult); err != nil {
			return fmt.Errorf("failed adding inspection rules: %w", err)
		}

		pIDs, err := attachHooks(mgr, inspectionResult, filePath.HostPath, filePath.ID)
		if err != nil {
			removeInspectionResultFromMap(offsetsDataMap, filePath.ID)
			return fmt.Errorf("error while attaching hooks to %s: %w", filePath.HostPath, err)
		}
		*probeIDs = pIDs

		elapsed := time.Since(start)

		binAnalysisMetric.Add(elapsed.Milliseconds())
		log.Debugf("attached hooks on %s (%v) in %s", filePath.HostPath, filePath.ID, elapsed)
		return nil
	}
}

func (p *goTLSProgram) handleProcessExit(pid pid) {
	_ = p.DetachPID(pid)
}

func (p *goTLSProgram) handleProcessStart(pid pid) {
	_ = p.AttachPID(pid)
}

// addInspectionResultToMap runs a binary inspection and adds the result to the
// map that's being read by the probes, indexed by the binary's inode number `ino`.
func addInspectionResultToMap(offsetsDataMap *ebpf.Map, binID utils.PathIdentifier, result *bininspect.Result) error {
	offsetsData, err := inspectionResultToProbeData(result)
	if err != nil {
		return fmt.Errorf("error while parsing inspection result: %w", err)
	}

	key := &gotls.TlsBinaryId{
		Id_major: unix.Major(binID.Dev),
		Id_minor: unix.Minor(binID.Dev),
		Ino:      binID.Inode,
	}
	if err := offsetsDataMap.Put(unsafe.Pointer(key), unsafe.Pointer(&offsetsData)); err != nil {
		return fmt.Errorf("could not write binary inspection result to map for binID %v: %w", binID, err)
	}

	return nil
}

func removeInspectionResultFromMap(offsetsDataMap *ebpf.Map, binID utils.PathIdentifier) {
	key := &gotls.TlsBinaryId{
		Id_major: unix.Major(binID.Dev),
		Id_minor: unix.Minor(binID.Dev),
		Ino:      binID.Inode,
	}
	if err := offsetsDataMap.Delete(unsafe.Pointer(key)); err != nil {
		log.Errorf("could not remove inspection result from map for ino %v: %s", binID, err)
	}
}

func attachHooks(mgr *manager.Manager, result *bininspect.Result, binPath string, binID utils.PathIdentifier) ([]manager.ProbeIdentificationPair, error) {
	uid := getUID(binID)
	probeIDs := make([]manager.ProbeIdentificationPair, 0)

	for function, uprobes := range functionToProbes {
		if functionsConfig[function].IncludeReturnLocations {
			if uprobes.returnInfo == "" {
				return nil, fmt.Errorf("function %q configured to include return locations but no return uprobes found in config", function)
			}
			for i, offset := range result.Functions[function].ReturnLocations {
				returnProbeID := manager.ProbeIdentificationPair{
					EBPFFuncName: uprobes.returnInfo,
					UID:          makeReturnUID(uid, i),
				}
				newProbe := &manager.Probe{
					ProbeIdentificationPair: returnProbeID,
					BinaryPath:              binPath,
					// Each return probe needs to have a unique uid value,
					// so add the index to the binary UID to make an overall UID.
					UprobeOffset: offset,
				}
				if err := mgr.AddHook("", newProbe); err != nil {
					return nil, fmt.Errorf("could not add return hook to function %q in offset %d due to: %w", function, offset, err)
				}
				probeIDs = append(probeIDs, returnProbeID)
				ddebpf.AddProgramNameMapping(newProbe.ID(), newProbe.EBPFFuncName, "usm_gotls")
			}
		}

		if uprobes.functionInfo != "" {
			probeID := manager.ProbeIdentificationPair{
				EBPFFuncName: uprobes.functionInfo,
				UID:          uid,
			}

			newProbe := &manager.Probe{
				BinaryPath:              binPath,
				UprobeOffset:            result.Functions[function].EntryLocation,
				ProbeIdentificationPair: probeID,
			}
			if err := mgr.AddHook("", newProbe); err != nil {
				return nil, fmt.Errorf("could not add hook for %q in offset %d due to: %w", uprobes.functionInfo, result.Functions[function].EntryLocation, err)
			}
			probeIDs = append(probeIDs, probeID)
			ddebpf.AddProgramNameMapping(newProbe.ID(), newProbe.EBPFFuncName, "usm_gotls")
		}
	}

	return probeIDs, nil
}

func unregisterCBCreator(mgr *manager.Manager, probeIDs *[]manager.ProbeIdentificationPair, offsetsDataMap *ebpf.Map) func(path utils.FilePath) error {
	return func(path utils.FilePath) error {
		if len(*probeIDs) == 0 {
			return nil
		}
		removeInspectionResultFromMap(offsetsDataMap, path.ID)
		for _, probeID := range *probeIDs {
			err := mgr.DetachHook(probeID)
			if err != nil {
				log.Errorf("failed detaching hook %s: %s", probeID.UID, err)
			}
		}
		log.Debugf("detached hooks on ino %v", path.ID)
		return nil
	}
}
