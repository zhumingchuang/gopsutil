// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64

package resourcecollector

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/windows"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

type nicBaseline struct {
	at     time.Time
	rx, tx uint64
}
type nodeSource struct {
	q    *pdhQuery
	at   time.Time
	nics map[string]nicBaseline
}

func newNodeSource() *nodeSource {
	q, _ := newPDHQuery(map[string]string{"cpu": `\Processor Information(_Total)\% Processor Time`, "idle": `\PhysicalDisk(*)\% Idle Time`, "read": `\PhysicalDisk(*)\Disk Read Bytes/sec`, "write": `\PhysicalDisk(*)\Disk Write Bytes/sec`, "commit": `\Memory\Committed Bytes`, "limit": `\Memory\Commit Limit`, "dedicated": `\GPU Adapter Memory(*)\Dedicated Usage`, "shared": `\GPU Adapter Memory(*)\Shared Usage`})
	return &nodeSource{q: q, nics: map[string]nicBaseline{}}
}
func nodeMetric(source string, at time.Time) Metric {
	return Metric{Status: OK, Source: source, CollectedAt: at}
}
func (n *nodeSource) capture(ctx context.Context, cores uint32, processGPU map[uint32]ProcessGPU) NodeSnapshot {
	at := time.Now()
	elapsed := at.Sub(n.at)
	valid := !n.at.IsZero() && elapsed > 0 && elapsed <= SnapshotTTL
	collected := false
	s := NodeSnapshot{CollectedAt: at, LogicalProcessors: cores, Volumes: []Volume{}, Disks: []PhysicalDisk{}, Network: []NetworkInterface{}, GPU: []NodeGPU{}}
	s.CPU.Metric = unavailable(Failed, "windows.pdh.cpu", "PDH_COLLECTION_FAILED", at)
	s.DisksMetric = unavailable(Failed, "windows.pdh.physicalDisk", "PDH_COLLECTION_FAILED", at)
	s.GPUMetric = unavailable(Unsupported, "windows.pdh.gpu", "GPU_COUNTER_UNAVAILABLE", at)
	if n.q != nil && n.q.collect() == nil {
		collected = true
		read := func(key string, integer bool) counterValues { return n.q.read(key, integer) }
		cpu := read("cpu", false)
		if bits, ok := cpu.Values["_Total"]; ok && cpu.Err == nil {
			s.CPU.Metric = nodeMetric("windows.pdh.cpu", at)
			if valid {
				s.CPU.Percent = validPercent(math.Float64frombits(bits))
				s.CPU.IntervalMs = float64(elapsed) / float64(time.Millisecond)
				if s.CPU.Percent == nil {
					s.CPU.Status = Partial
					s.CPU.ErrorCode = "CPU_COUNTER_INVALID"
				}
			} else {
				s.CPU.Status = WarmingUp
			}
		}
		idle, rd, wr := read("idle", false), read("read", false), read("write", false)
		s.DisksMetric = nodeMetric("windows.pdh.physicalDisk", at)
		if idle.Err != nil || rd.Err != nil || wr.Err != nil {
			s.DisksMetric = unavailable(Partial, "windows.pdh.physicalDisk", "PDH_COUNTER_UNAVAILABLE", at)
		}
		for name, bits := range idle.Values {
			if name == "_Total" {
				continue
			}
			d := PhysicalDisk{Metric: s.DisksMetric, Instance: name}
			d.IntervalMs = float64(elapsed) / float64(time.Millisecond)
			if valid {
				d.BusyPercent = validPercent(100 - math.Float64frombits(bits))
				if b, ok := rd.Values[name]; ok {
					d.ReadBytesPerSecond = validRate(math.Float64frombits(b))
				}
				if b, ok := wr.Values[name]; ok {
					d.WriteBytesPerSecond = validRate(math.Float64frombits(b))
				}
				if d.BusyPercent == nil || d.ReadBytesPerSecond == nil || d.WriteBytesPerSecond == nil {
					d.Status = Partial
					d.ErrorCode = "PDH_VALUE_UNAVAILABLE"
				}
			} else {
				d.Status = WarmingUp
			}
			s.Disks = append(s.Disks, d)
		}
		commit, limit := read("commit", true), read("limit", true)
		for _, b := range commit.Values {
			s.Memory.CommitTotalBytes = ptr(b)
		}
		for _, b := range limit.Values {
			s.Memory.CommitLimitBytes = ptr(b)
		}
	}
	s.Memory.Metric = nodeMetric("windows.GlobalMemoryStatusEx", at)
	if m, err := mem.VirtualMemoryWithContext(ctx); err == nil && m.Total > 0 {
		s.Memory.TotalBytes = ptr(m.Total)
		s.Memory.AvailableBytes = ptr(m.Available)
		s.Memory.UsedPercent = ptr(float64(m.Total-m.Available) / float64(m.Total) * 100)
	} else {
		s.Memory.Metric = unavailable(Failed, "windows.GlobalMemoryStatusEx", "MEMORY_QUERY_FAILED", at)
	}
	s.Volumes, s.VolumesMetric = readVolumes(at)
	s.NetworkMetric = nodeMetric("windows.GetIfEntry2Ex", at)
	next := map[string]nicBaseline{}
	interfaces, err := net.Interfaces()
	if err != nil {
		s.NetworkMetric = unavailable(Failed, "windows.GetIfEntry2Ex", "INTERFACE_ENUM_FAILED", at)
	}
	for _, nic := range interfaces {
		row := windows.MibIfRow2{InterfaceIndex: uint32(nic.Index)}
		if windows.GetIfEntry2Ex(0, &row) != nil {
			s.NetworkMetric = unavailable(Partial, "windows.GetIfEntry2Ex", "INTERFACE_QUERY_FAILED", at)
			continue
		}
		id := row.InterfaceGuid.String()
		v := NetworkInterface{Metric: nodeMetric("windows.GetIfEntry2Ex", at), ID: id, LUID: row.InterfaceLuid, Index: row.InterfaceIndex, Name: windows.UTF16ToString(row.Alias[:]), Description: windows.UTF16ToString(row.Description[:]), Type: row.Type, Hardware: row.InterfaceAndOperStatusFlags&1 != 0, OperStatus: row.OperStatus, RxLinkBitsPerSecond: row.ReceiveLinkSpeed, TxLinkBitsPerSecond: row.TransmitLinkSpeed, RxBytesTotal: row.InOctets, TxBytesTotal: row.OutOctets}
		old, ok := n.nics[id]
		dt := at.Sub(old.at)
		if ok && dt > 0 && dt <= SnapshotTTL && row.InOctets >= old.rx && row.OutOctets >= old.tx {
			rx, tx := float64(row.InOctets-old.rx)/dt.Seconds(), float64(row.OutOctets-old.tx)/dt.Seconds()
			v.RxBytesPerSecond = &rx
			v.TxBytesPerSecond = &tx
			v.IntervalMs = float64(dt) / float64(time.Millisecond)
			if row.ReceiveLinkSpeed > 0 {
				v.RxPercent = ptr(rx * 800 / float64(row.ReceiveLinkSpeed))
			}
			if row.TransmitLinkSpeed > 0 {
				v.TxPercent = ptr(tx * 800 / float64(row.TransmitLinkSpeed))
			}
		} else {
			v.Status = WarmingUp
		}
		next[id] = nicBaseline{at, row.InOctets, row.OutOctets}
		s.Network = append(s.Network, v)
	}
	n.nics = next
	// The same PDH sample window is used for all process engine instances. Sum
	// each physical engine, then take the busiest engine of each adapter.
	engines := map[string]map[uint32]float64{}
	gpuStatus := map[string]Status{}
	for _, g := range processGPU {
		for _, e := range g.Engines {
			id := fmt.Sprintf("%s:%d", e.AdapterLUID, e.PhysicalAdapterIndex)
			if engines[id] == nil {
				engines[id] = map[uint32]float64{}
			}
			engines[id][e.EngineID] += e.Percent
			if old, exists := gpuStatus[id]; !exists || old == OK {
				gpuStatus[id] = g.Status
			}
		}
	}
	mems := map[string]*NodeGPU{}
	if n.q != nil {
		for _, field := range []string{"dedicated", "shared"} {
			values := n.q.read(field, true)
			for name, b := range values.Values {
				match := adapterInstance.FindStringSubmatch(name)
				if match == nil {
					continue
				}
				hi, _ := strconv.ParseUint(match[1], 16, 32)
				lo, _ := strconv.ParseUint(match[2], 16, 32)
				id := fmt.Sprintf("%08x:%08x:%s", hi, lo, match[3])
				if mems[id] == nil {
					mems[id] = &NodeGPU{Metric: nodeMetric("windows.pdh.gpu", at), ID: id}
				}
				if field == "dedicated" {
					mems[id].DedicatedBytes = ptr(b)
				} else {
					mems[id].SharedBytes = ptr(b)
				}
			}
		}
	}
	for id, values := range engines {
		if mems[id] == nil {
			mems[id] = &NodeGPU{Metric: nodeMetric("windows.pdh.gpu", at), ID: id}
		}
		maximum := 0.0
		for _, v := range values {
			maximum = math.Max(maximum, v)
		}
		mems[id].Percent = ptr(math.Min(100, maximum))
		if gpuStatus[id] != OK {
			mems[id].Status = Partial
		}
	}
	for _, g := range mems {
		if g.Percent == nil {
			g.Status = Partial
			g.ErrorCode = "GPU_ENGINE_UNAVAILABLE"
		}
		s.GPU = append(s.GPU, *g)
	}
	if len(s.GPU) > 0 {
		s.GPUMetric = nodeMetric("windows.pdh.gpu", at)
	}
	sort.Slice(s.Network, func(i, j int) bool { return s.Network[i].ID < s.Network[j].ID })
	sort.Slice(s.Disks, func(i, j int) bool { return s.Disks[i].Instance < s.Disks[j].Instance })
	sort.Slice(s.GPU, func(i, j int) bool { return s.GPU[i].ID < s.GPU[j].ID })
	if collected {
		n.at = at
	} else {
		n.at = time.Time{}
	}
	return s
}

var adapterInstance = regexp.MustCompile(`^luid_0x([0-9a-fA-F]+)_0x([0-9a-fA-F]+)_phys_(\d+)$`)

func validPercent(v float64) *float64 {
	if !finite(v) || v < 0 || v > 100.01 {
		return nil
	}
	return ptr(math.Min(v, 100))
}
func validRate(v float64) *float64 {
	if !finite(v) || v < 0 {
		return nil
	}
	return &v
}

type diskSpace struct {
	Total, Available, PoolUnavailable, CallerTotal, CallerAvailable, CallerPoolUnavailable, Used, Reserved, StorageReserve, Committed, PoolAvailable uint64
	Sectors, Bytes                                                                                                                                   uint32
}

var diskSpaceProc = kernelDLL.NewProc("GetDiskSpaceInformationW")

func readVolumes(at time.Time) ([]Volume, Metric) {
	metric := nodeMetric("windows.GetDiskSpaceInformationW", at)
	out := []Volume{}
	buf := make([]uint16, 1024)
	h, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		return out, unavailable(Failed, metric.Source, "VOLUME_ENUM_FAILED", at)
	}
	defer windows.FindVolumeClose(h)
	for {
		id := windows.UTF16ToString(buf)
		numbers, mapping := volumeDisks(id, at)
		v := Volume{Metric: metric, ID: id, MountPaths: []string{}, DiskNumbers: numbers, DiskMapping: mapping}
		paths := make([]uint16, 32768)
		var size uint32
		if windows.GetVolumePathNamesForVolumeName(&buf[0], &paths[0], uint32(len(paths)), &size) == nil {
			start := 0
			for i, c := range paths {
				if c == 0 {
					if i == start {
						break
					}
					v.MountPaths = append(v.MountPaths, windows.UTF16ToString(paths[start:i]))
					start = i + 1
				}
			}
		}
		if diskSpaceProc.Find() != nil {
			v.Metric = unavailable(Unsupported, metric.Source, "DISK_SPACE_API_UNAVAILABLE", at)
		} else {
			var space diskSpace
			hr, _, _ := diskSpaceProc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&space)))
			unit := uint64(space.Sectors) * uint64(space.Bytes)
			if int32(hr) < 0 || unit == 0 || space.Total > math.MaxUint64/unit || space.Available > space.Total || space.CallerAvailable > math.MaxUint64/unit {
				v.Metric = unavailable(Failed, metric.Source, "VOLUME_SPACE_QUERY_FAILED", at)
			} else {
				total, free, available := space.Total*unit, space.Available*unit, space.CallerAvailable*unit
				v.TotalBytes = &total
				v.FreeBytes = &free
				v.CallerAvailableBytes = &available
				if total > 0 {
					v.UsedPercent = ptr(float64(total-free) / float64(total) * 100)
				}
			}
		}
		if v.Status != OK {
			metric.Status = Partial
			metric.ErrorCode = "VOLUME_PARTIAL"
		}
		out = append(out, v)
		if len(out) >= 256 {
			metric = unavailable(Partial, metric.Source, "VOLUME_LIMIT", at)
			break
		}
		if err = windows.FindNextVolume(h, &buf[0], uint32(len(buf))); err != nil {
			if err != windows.ERROR_NO_MORE_FILES {
				metric = unavailable(Partial, metric.Source, "VOLUME_ENUM_FAILED", at)
			}
			break
		}
	}
	return out, metric
}

func volumeDisks(id string, at time.Time) ([]uint32, Metric) {
	numbers := []uint32{}
	metric := nodeMetric("windows.IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS", at)
	name, err := windows.UTF16PtrFromString(strings.TrimSuffix(id, `\`))
	if err != nil {
		return numbers, unavailable(Failed, metric.Source, "VOLUME_PATH_INVALID", at)
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return numbers, nativeError(err, metric.Source, at)
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 4096)
	var size uint32
	err = windows.DeviceIoControl(h, 0x560000, nil, 0, &buf[0], uint32(len(buf)), &size, nil)
	if err != nil {
		return numbers, nativeError(err, metric.Source, at)
	}
	if size < 8 {
		return numbers, unavailable(Failed, metric.Source, "DISK_EXTENTS_INVALID", at)
	}
	count := binary.LittleEndian.Uint32(buf[:4])
	if count > 64 || 8+uint64(count)*24 > uint64(size) {
		return numbers, unavailable(Partial, metric.Source, "DISK_EXTENTS_LIMIT", at)
	}
	seen := map[uint32]bool{}
	for i := uint32(0); i < count; i++ {
		n := binary.LittleEndian.Uint32(buf[8+i*24:])
		if !seen[n] {
			numbers = append(numbers, n)
			seen[n] = true
		}
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	return numbers, metric
}
