// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64

package resourcecollector

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var kernelDLL = windows.NewLazySystemDLL("kernel32.dll")
var activeProcessors = kernelDLL.NewProc("GetActiveProcessorCount")
var processIO = kernelDLL.NewProc("GetProcessIoCounters")
var processMemory = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
var processName = kernelDLL.NewProc("QueryFullProcessImageNameW")

type processMemoryCounters struct {
	Size, Faults                                                                                               uint32
	PeakWorkingSet, WorkingSet, PeakPaged, Paged, PeakNonPaged, NonPaged, Pagefile, PeakPagefile, PrivateUsage uint64
}
type ioCounters struct{ ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64 }
type processMemoryCountersEx2 struct {
	processMemoryCounters
	PrivateWorkingSet, SharedCommit uint64
}

type WindowsSource struct {
	node                       *nodeSource
	etw                        *etwSource
	mu                         sync.Mutex
	pdh                        *pdhQuery
	pdhError                   error
	lastPDH                    time.Time
	lastIdentities             map[uint32]uint64
	closed                     bool
	privateWorkingSetSupported bool
}

func NewWindowsSource() (*WindowsSource, error) {
	q, err := newPDHQuery(map[string]string{
		"engine": `\GPU Engine(*)\Utilization Percentage`, "dedicated": `\GPU Process Memory(*)\Dedicated Usage`, "shared": `\GPU Process Memory(*)\Shared Usage`,
	})
	return &WindowsSource{node: newNodeSource(), etw: newETWSource(), pdh: q, pdhError: err, lastIdentities: make(map[uint32]uint64), privateWorkingSetSupported: supportsPrivateWorkingSet()}, nil
}

func (s *WindowsSource) Capabilities() Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	gpu := Capability{Supported: true, Source: "windows.pdh.gpu"}
	if s.pdh == nil {
		gpu.Supported = false
		gpu.Reason = "PDH_UNAVAILABLE"
	} else if _, ok := s.pdh.counters["engine"]; !ok {
		gpu.Supported = false
		gpu.Reason = "GPU_COUNTER_UNAVAILABLE"
	}
	return Capabilities{SchemaVersion: SchemaVersion, ABIVersion: 1, Platform: "windows/amd64", CPU: Capability{Supported: true, Source: "windows.GetProcessTimes"},
		Memory: Capability{Supported: true, Source: "windows.GetProcessMemoryInfo"}, GenericIO: Capability{Supported: true, Source: "windows.GetProcessIoCounters"}, GPU: gpu,
		Disk: s.etw.capability("windows.etw.disk"), Network: s.etw.capability("windows.etw.network"),
		TreeGroups: true, SessionGroups: false, MinimumIntervalMs: 1000, MaximumProcesses: MaximumProcesses}
}

func (s *WindowsSource) Capture(ctx context.Context) (RawSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RawSample{}, ErrClosed
	}
	result := RawSample{StartedAt: time.Now(), EnumerationComplete: true, Processes: []RawProcess{}}
	count, _, _ := activeProcessors.Call(0xffff)
	if count == 0 || count > 65535 {
		return result, fmt.Errorf("CPU_COUNT_INVALID")
	}
	result.LogicalProcessors = uint32(count)
	if memory, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		result.TotalPhysicalBytes = memory.Total
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return result, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if e := ctx.Err(); e != nil {
			return RawSample{}, e
		}
		result.TotalEnumerated++
		if len(result.Processes) < MaximumProcesses {
			p := readNativeProcess(entry, s.privateWorkingSetSupported)
			// An entry may have been enumerated before its PID was recycled.
			// Only pre-existing births may use Toolhelp's parent association.
			p.ParentVerified = p.CreationTime != 0 && p.CreationTime <= filetime(result.StartedAt)
			result.Processes = append(result.Processes, p)
		} else {
			result.EnumerationComplete = false
			result.LimitReason = "PROCESS_COUNT_LIMIT"
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return RawSample{}, err
	}
	pdhAt := time.Now()
	gpus := map[uint32]ProcessGPU{}
	var pdhErr error
	if s.pdh != nil {
		pdhErr = s.pdh.collect()
		if pdhErr == nil {
			gpus = readProcessGPUs(s.pdh, pdhAt, s.lastPDH)
		}
	} else {
		pdhErr = s.pdhError
	}
	next := make(map[uint32]uint64, len(result.Processes))
	for i := range result.Processes {
		p := &result.Processes[i]
		if p.Identity.Status != OK {
			continue
		}
		// A counter only carries PID. Re-open and verify the birth time after
		// PDH collection before joining GPU values to a row.
		birth, err := readBirth(p.PID)
		if err != nil || birth != p.CreationTime {
			p.Identity = unavailable(Failed, "windows.GetProcessTimes", "PROCESS_CHANGED_DURING_CAPTURE", p.At)
			p.CreationTime = 0
			p.Memory = ProcessMemory{Metric: p.Identity}
			p.GenericIO = ProcessIO{Metric: p.Identity}
			p.GPU = ProcessGPU{Metric: p.Identity, Engines: []GPUEngine{}, Memory: []GPUMemory{}}
			continue
		}
		next[p.PID] = birth
		gpu, exists := gpus[p.PID]
		if pdhErr != nil {
			p.GPU = ProcessGPU{Metric: unavailable(Failed, "windows.pdh.gpu", "PDH_COLLECTION_FAILED", pdhAt), Engines: []GPUEngine{}, Memory: []GPUMemory{}}
			continue
		}
		if exists {
			p.GPU = gpu
		}
		if s.lastPDH.IsZero() || s.lastIdentities[p.PID] != birth || birth > filetime(s.lastPDH) {
			p.GPU.Status = WarmingUp
			p.GPU.ErrorCode = "GPU_IDENTITY_WINDOW_WARMING_UP"
			p.GPU.Percent = nil
			p.GPU.BusiestEngine = nil
			p.GPU.Engines = []GPUEngine{}
		}
	}
	if pdhErr == nil {
		s.lastPDH = pdhAt
		s.lastIdentities = next
	} else {
		s.lastPDH = time.Time{}
		s.lastIdentities = make(map[uint32]uint64)
	}
	s.etw.clockCheck()
	s.etw.capture(result.Processes)
	result.Node = s.node.capture(ctx, result.LogicalProcessors, gpus)
	result.CompletedAt = time.Now()
	wallElapsed := result.CompletedAt.UTC().Sub(result.StartedAt.UTC())
	monoElapsed := result.CompletedAt.Sub(result.StartedAt)
	if difference := wallElapsed - monoElapsed; difference > time.Millisecond || difference < -time.Millisecond {
		for i := range result.Processes {
			result.Processes[i].ParentVerified = false
		}
	}
	return result, nil
}

func readNativeProcess(entry windows.ProcessEntry32, privateSupported bool) RawProcess {
	at := time.Now()
	p := RawProcess{PID: entry.ProcessID, ParentPID: entry.ParentProcessID, Name: windows.UTF16ToString(entry.ExeFile[:]), At: at,
		Identity:  Metric{Status: OK, Source: "windows.GetProcessTimes", CollectedAt: at},
		Memory:    ProcessMemory{Metric: unavailable(Partial, "windows.psapi", "PRIVATE_WORKING_SET_UNAVAILABLE", at)},
		GenericIO: ProcessIO{Metric: Metric{Status: OK, Source: "windows.GetProcessIoCounters", CollectedAt: at}},
		GPU:       ProcessGPU{Metric: unavailable(Unsupported, "windows.pdh.gpu", "GPU_INSTANCE_UNAVAILABLE", at), Engines: []GPUEngine{}, Memory: []GPUMemory{}}}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.PID)
	if err != nil {
		p.Identity = nativeError(err, "windows.OpenProcess", at)
		p.Memory.Metric = p.Identity
		p.GenericIO.Metric = p.Identity
		p.GPU.Metric = p.Identity
		return p
	}
	defer windows.CloseHandle(handle)
	var birth, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &birth, &exit, &kernel, &user); err != nil {
		p.Identity = nativeError(err, "windows.GetProcessTimes", at)
		return p
	}
	p.CreationTime = uint64(birth.HighDateTime)<<32 | uint64(birth.LowDateTime)
	p.UserTime = uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	p.KernelTime = uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	if exit.HighDateTime != 0 || exit.LowDateTime != 0 {
		p.Identity = unavailable(Failed, "windows.GetProcessTimes", "PROCESS_EXITED_DURING_CAPTURE", at)
		p.CreationTime = 0
		return p
	}
	nameBuffer := make([]uint16, 32768)
	length := uint32(len(nameBuffer))
	ok, _, _ := processName.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&nameBuffer[0])), uintptr(unsafe.Pointer(&length)))
	if ok != 0 {
		p.Name = filepath.Base(windows.UTF16ToString(nameBuffer[:length]))
	}
	var io ioCounters
	ok, _, ioErr := processIO.Call(uintptr(handle), uintptr(unsafe.Pointer(&io)))
	if ok != 0 {
		p.GenericIO.ReadBytesTotal = ptr(io.ReadBytes)
		p.GenericIO.WriteBytesTotal = ptr(io.WriteBytes)
	} else {
		p.GenericIO.Metric = nativeError(ioErr, "windows.GetProcessIoCounters", at)
	}
	// PSAPI working-set access can require more rights than the birth/CPU query.
	// Keep the successfully read identity and CPU when memory access is denied.
	memHandle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, p.PID)
	if err != nil {
		p.Memory.Metric = nativeError(err, "windows.psapi", at)
		return p
	}
	defer windows.CloseHandle(memHandle)
	var secondBirth, secondExit, k2, u2 windows.Filetime
	if windows.GetProcessTimes(memHandle, &secondBirth, &secondExit, &k2, &u2) != nil || secondBirth != birth {
		p.Memory.Metric = unavailable(Failed, "windows.psapi", "PROCESS_IDENTITY_CHANGED", at)
		return p
	}
	var counters processMemoryCountersEx2
	counters.Size = uint32(unsafe.Sizeof(counters.processMemoryCounters))
	if privateSupported {
		counters.Size = uint32(unsafe.Sizeof(counters))
	}
	ok, _, memErr := processMemory.Call(uintptr(memHandle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
	if ok != 0 {
		p.Memory.WorkingSetBytes = ptr(counters.WorkingSet)
		p.Memory.PrivateCommitBytes = ptr(counters.PrivateUsage)
		if privateSupported {
			p.Memory.PrivateWorkingSetBytes = ptr(counters.PrivateWorkingSet)
			p.Memory.Status = OK
			p.Memory.ErrorCode = ""
			p.Memory.Source = "windows.GetProcessMemoryInfo.EX2"
		}
	} else {
		p.Memory.Metric = nativeError(memErr, "windows.psapi", at)
	}
	return p
}

func readBirth(pid uint32) (uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var b, e, k, u windows.Filetime
	if err = windows.GetProcessTimes(h, &b, &e, &k, &u); err != nil {
		return 0, err
	}
	if e.HighDateTime != 0 || e.LowDateTime != 0 {
		return 0, fmt.Errorf("PROCESS_EXITED")
	}
	return uint64(b.HighDateTime)<<32 | uint64(b.LowDateTime), nil
}
func filetime(t time.Time) uint64 {
	return uint64(t.Unix()+11644473600)*10000000 + uint64(t.Nanosecond()/100)
}
func nativeError(err error, source string, at time.Time) Metric {
	status := Failed
	code := "NATIVE_QUERY_FAILED"
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		status = PermissionDenied
		code = "PROCESS_ACCESS_DENIED"
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		code = "PROCESS_NOT_FOUND"
	}
	return unavailable(status, source, code, at)
}
func (s *WindowsSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.etw.close()
	if s.node.q != nil {
		s.node.q.close()
	}
	return s.pdh.close()
}

func supportsPrivateWorkingSet() bool {
	version := windows.RtlGetVersion()
	if version.MajorVersion < 10 {
		return false
	}
	if version.BuildNumber >= 22631 {
		return true
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return false
	}
	defer key.Close()
	revision, _, err := key.GetIntegerValue("UBR")
	if err != nil {
		return false
	}
	return version.BuildNumber == 22621 && revision >= 2361 || version.BuildNumber == 19045 && revision >= 3448
}
