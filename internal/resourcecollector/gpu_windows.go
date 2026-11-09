// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64

package resourcecollector

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var gpuEngineName = regexp.MustCompile(`^pid_(\d+)_luid_0x([0-9a-fA-F]+)_0x([0-9a-fA-F]+)_phys_(\d+)_eng_(\d+)_engtype_(.+)$`)
var gpuMemoryName = regexp.MustCompile(`^pid_(\d+)_luid_0x([0-9a-fA-F]+)_0x([0-9a-fA-F]+)_phys_(\d+)$`)

type gpuInstance struct {
	pid              uint32
	luid             string
	physical, engine uint32
	engineType       string
}

func parseGPUInstance(name string, engine bool) (gpuInstance, error) {
	pattern := gpuMemoryName
	if engine {
		pattern = gpuEngineName
	}
	parts := pattern.FindStringSubmatch(name)
	if parts == nil {
		return gpuInstance{}, fmt.Errorf("GPU_INSTANCE_INVALID")
	}
	pid, e1 := strconv.ParseUint(parts[1], 10, 32)
	hi, e2 := strconv.ParseUint(parts[2], 16, 32)
	lo, e3 := strconv.ParseUint(parts[3], 16, 32)
	phys, e4 := strconv.ParseUint(parts[4], 10, 32)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return gpuInstance{}, fmt.Errorf("GPU_INSTANCE_INVALID")
	}
	i := gpuInstance{pid: uint32(pid), luid: fmt.Sprintf("%08x:%08x", hi, lo), physical: uint32(phys)}
	if engine {
		eng, err := strconv.ParseUint(parts[5], 10, 32)
		if err != nil {
			return gpuInstance{}, err
		}
		i.engine = uint32(eng)
		i.engineType = parts[6]
	}
	return i, nil
}

func readProcessGPUs(q *pdhQuery, at, previous time.Time) map[uint32]ProcessGPU {
	engines, dedicated, shared := q.read("engine", false), q.read("dedicated", true), q.read("shared", true)
	return assembleProcessGPUs(engines, dedicated, shared, at, previous)
}

func assembleProcessGPUs(engines, dedicated, shared counterValues, at, previous time.Time) map[uint32]ProcessGPU {
	out := make(map[uint32]ProcessGPU)
	get := func(pid uint32) ProcessGPU {
		if p, ok := out[pid]; ok {
			return p
		}
		return ProcessGPU{Metric: Metric{Status: OK, Source: "windows.pdh.gpu", CollectedAt: at, IntervalMs: float64(at.Sub(previous)) / float64(time.Millisecond)}, Engines: []GPUEngine{}, Memory: []GPUMemory{}}
	}
	for name, bits := range engines.Values {
		i, err := parseGPUInstance(name, true)
		if err != nil {
			continue
		}
		percent := pdhDouble(bits)
		if !finite(percent) || percent < 0 || percent > 100.01 {
			continue
		}
		p := get(i.pid)
		p.Engines = append(p.Engines, GPUEngine{AdapterLUID: i.luid, PhysicalAdapterIndex: i.physical, EngineID: i.engine, EngineType: i.engineType, Percent: min(percent, 100)})
		out[i.pid] = p
	}
	names := make(map[string]bool)
	for n := range dedicated.Values {
		names[n] = true
	}
	for n := range shared.Values {
		names[n] = true
	}
	for name := range names {
		i, err := parseGPUInstance(name, false)
		if err != nil {
			continue
		}
		p := get(i.pid)
		m := GPUMemory{AdapterLUID: i.luid, PhysicalAdapterIndex: i.physical}
		if v, ok := dedicated.Values[name]; ok {
			m.DedicatedBytes = ptr(v)
		}
		if v, ok := shared.Values[name]; ok {
			m.SharedBytes = ptr(v)
		}
		p.Memory = append(p.Memory, m)
		out[i.pid] = p
	}
	for pid, p := range out {
		sort.Slice(p.Engines, func(i, j int) bool {
			a, b := p.Engines[i], p.Engines[j]
			if a.AdapterLUID != b.AdapterLUID {
				return a.AdapterLUID < b.AdapterLUID
			}
			if a.PhysicalAdapterIndex != b.PhysicalAdapterIndex {
				return a.PhysicalAdapterIndex < b.PhysicalAdapterIndex
			}
			return a.EngineID < b.EngineID
		})
		sort.Slice(p.Memory, func(i, j int) bool {
			if p.Memory[i].AdapterLUID != p.Memory[j].AdapterLUID {
				return p.Memory[i].AdapterLUID < p.Memory[j].AdapterLUID
			}
			return p.Memory[i].PhysicalAdapterIndex < p.Memory[j].PhysicalAdapterIndex
		})
		for i := range p.Engines {
			engine := p.Engines[i]
			if p.Percent == nil || engine.Percent > *p.Percent {
				p.Percent = ptr(engine.Percent)
				p.BusiestEngine = &engine
			}
		}
		if engines.Err != nil || dedicated.Err != nil || shared.Err != nil {
			p.Status = Partial
			p.ErrorCode = "GPU_COUNTER_PARTIAL"
		}
		if p.Percent == nil && p.Status == OK {
			p.Status = Partial
			p.ErrorCode = "GPU_ENGINE_UNAVAILABLE"
		}
		if previous.IsZero() || at.Sub(previous) <= 0 || at.Sub(previous) > 15*time.Second {
			p.Status = WarmingUp
			p.IntervalMs = 0
			p.Percent = nil
			p.BusiestEngine = nil
			p.Engines = []GPUEngine{}
		}
		out[pid] = p
	}
	return out
}
