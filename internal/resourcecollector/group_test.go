// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"testing"
	"time"
)

func TestGroupGPUUsesAlignedEngineSumsAndETWIsNeverGenericIO(t *testing.T) {
	at := time.Now()
	m := Metric{Status: OK, Source: "test", CollectedAt: at, IntervalMs: 2000}
	makeRow := func(v float64) ProcessUsage {
		return ProcessUsage{Disk: ProcessIO{Metric: m, ReadBytesTotal: ptr(uint64(10)), WriteBytesTotal: ptr(uint64(20)), ReadBytesPerSecond: ptr(5.0), WriteBytesPerSecond: ptr(10.0)}, Network: ProcessNetwork{Metric: m, RxBytesSinceEpoch: ptr(uint64(30)), TxBytesSinceEpoch: ptr(uint64(40)), RxBytesPerSecond: ptr(15.0), TxBytesPerSecond: ptr(20.0)}, GPU: ProcessGPU{Metric: m, Engines: []GPUEngine{{AdapterLUID: "adapter", EngineID: 1, Percent: v}, {AdapterLUID: "adapter", EngineID: 2, Percent: 30}}, Memory: []GPUMemory{}}}
	}
	rows := map[uint32]ProcessUsage{1: makeRow(40), 2: makeRow(30)}
	g := ProcessGroup{MembershipComplete: true}
	aggregateGroupResources(&g, rows, at)
	if g.GPU.Percent == nil || *g.GPU.Percent != 70 || g.Disk.ReadBytesTotal == nil || *g.Disk.ReadBytesTotal != 20 || *g.Network.RxBytesPerSecond != 30 {
		t.Fatalf("%+v", g)
	}
	r := rows[2]
	r.Network.Status = PermissionDenied
	r.GPU.CollectedAt = at.Add(time.Second)
	rows[2] = r
	aggregateGroupResources(&g, rows, at)
	if g.Network.RxBytesPerSecond != nil || g.GPU.Percent != nil || g.Disk.Status != OK {
		t.Fatalf("mixed windows accepted: %+v", g)
	}
}
