// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Disk/network deltas share one ETW drain window. GPU percentages are summed
// per physical engine before selecting the busiest engine, never per process.
func aggregateGroupResources(group *ProcessGroup, members map[uint32]ProcessUsage, at time.Time) {
	disk := ProcessIO{Metric: Metric{Status: OK, Source: "windows.etw.disk.group", CollectedAt: at}}
	network := ProcessNetwork{Metric: Metric{Status: OK, Source: "windows.etw.network.group", CollectedAt: at}}
	gpu := ProcessGPU{Metric: Metric{Status: OK, Source: "windows.pdh.gpu.group", CollectedAt: at}, Engines: []GPUEngine{}, Memory: []GPUMemory{}}
	var read, write, rx, tx uint64
	var rd, wr, recv, send float64
	var diskAt, netAt, gpuAt time.Time
	engines := map[string]GPUEngine{}
	memory := map[string]GPUMemory{}
	for _, row := range members {
		d, n, g := row.Disk, row.Network, row.GPU
		if d.Status != OK || d.ReadBytesTotal == nil || d.WriteBytesTotal == nil || d.ReadBytesPerSecond == nil || d.WriteBytesPerSecond == nil || !diskAt.IsZero() && (!diskAt.Equal(d.CollectedAt) || disk.IntervalMs != d.IntervalMs) {
			disk.Status = Partial
		} else {
			if read > math.MaxUint64-*d.ReadBytesTotal || write > math.MaxUint64-*d.WriteBytesTotal {
				disk.Status = Partial
			} else {
				read += *d.ReadBytesTotal
				write += *d.WriteBytesTotal
			}
			rd += *d.ReadBytesPerSecond
			wr += *d.WriteBytesPerSecond
			diskAt = d.CollectedAt
			disk.IntervalMs = d.IntervalMs
		}
		if n.Status != OK || n.RxBytesSinceEpoch == nil || n.TxBytesSinceEpoch == nil || n.RxBytesPerSecond == nil || n.TxBytesPerSecond == nil || !netAt.IsZero() && (!netAt.Equal(n.CollectedAt) || network.IntervalMs != n.IntervalMs) {
			network.Status = Partial
		} else {
			if rx > math.MaxUint64-*n.RxBytesSinceEpoch || tx > math.MaxUint64-*n.TxBytesSinceEpoch {
				network.Status = Partial
			} else {
				rx += *n.RxBytesSinceEpoch
				tx += *n.TxBytesSinceEpoch
			}
			recv += *n.RxBytesPerSecond
			send += *n.TxBytesPerSecond
			netAt = n.CollectedAt
			network.IntervalMs = n.IntervalMs
		}
		if g.Status != OK || !gpuAt.IsZero() && (!gpuAt.Equal(g.CollectedAt) || gpu.IntervalMs != g.IntervalMs) {
			gpu.Status = Partial
		}
		gpuAt = g.CollectedAt
		gpu.IntervalMs = g.IntervalMs
		for _, e := range g.Engines {
			key := fmt.Sprintf("%s/%d/%d/%s", e.AdapterLUID, e.PhysicalAdapterIndex, e.EngineID, e.EngineType)
			v, exists := engines[key]
			if !exists {
				v = e
			} else {
				v.Percent += e.Percent
			}
			engines[key] = v
		}
		for _, m := range g.Memory {
			key := fmt.Sprintf("%s/%d", m.AdapterLUID, m.PhysicalAdapterIndex)
			v, exists := memory[key]
			if !exists {
				v = m
			} else {
				if v.DedicatedBytes != nil && m.DedicatedBytes != nil && *v.DedicatedBytes <= math.MaxUint64-*m.DedicatedBytes {
					v.DedicatedBytes = ptr(*v.DedicatedBytes + *m.DedicatedBytes)
				} else {
					v.DedicatedBytes = nil
					gpu.Status = Partial
				}
				if v.SharedBytes != nil && m.SharedBytes != nil && *v.SharedBytes <= math.MaxUint64-*m.SharedBytes {
					v.SharedBytes = ptr(*v.SharedBytes + *m.SharedBytes)
				} else {
					v.SharedBytes = nil
					gpu.Status = Partial
				}
			}
			memory[key] = v
		}
	}
	if !group.MembershipComplete {
		disk.Status = Partial
		network.Status = Partial
		gpu.Status = Partial
	}
	if disk.Status == OK {
		disk.ReadBytesTotal = &read
		disk.WriteBytesTotal = &write
		disk.ReadBytesPerSecond = &rd
		disk.WriteBytesPerSecond = &wr
	} else {
		disk.ErrorCode = "GROUP_MEMBERS_OR_WINDOW_INCOMPLETE"
	}
	if network.Status == OK {
		network.RxBytesSinceEpoch = &rx
		network.TxBytesSinceEpoch = &tx
		network.RxBytesPerSecond = &recv
		network.TxBytesPerSecond = &send
	} else {
		network.ErrorCode = "GROUP_MEMBERS_OR_WINDOW_INCOMPLETE"
	}
	keys := make([]string, 0, len(engines))
	for key := range engines {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	maximum := 0.0
	for _, key := range keys {
		e := engines[key]
		if e.Percent > 100.01 {
			gpu.Status = Partial
		}
		e.Percent = math.Min(e.Percent, 100)
		gpu.Engines = append(gpu.Engines, e)
		if e.Percent >= maximum {
			maximum = e.Percent
			copy := e
			gpu.BusiestEngine = &copy
		}
	}
	keys = keys[:0]
	for key := range memory {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		gpu.Memory = append(gpu.Memory, memory[key])
	}
	if gpu.Status == OK {
		gpu.Percent = &maximum
	} else {
		gpu.ErrorCode = "GROUP_MEMBERS_OR_WINDOW_INCOMPLETE"
		gpu.Percent = nil
		gpu.BusiestEngine = nil
	}
	group.Disk = disk
	group.Network = network
	group.GPU = gpu
}
