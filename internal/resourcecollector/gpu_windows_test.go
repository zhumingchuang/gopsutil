// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64

package resourcecollector

import (
	"math"
	"testing"
	"time"
)

func TestGPUInstanceParserPreservesAdapterAndEngine(t *testing.T) {
	i, err := parseGPUInstance("pid_123_luid_0x00000001_0x0000fe7b_phys_2_eng_3_engtype_VideoEncode", true)
	if err != nil || i.pid != 123 || i.luid != "00000001:0000fe7b" || i.physical != 2 || i.engine != 3 || i.engineType != "VideoEncode" {
		t.Fatalf("%+v %v", i, err)
	}
	for _, bad := range []string{"pid_1", "pid_4294967296_luid_0x0_0x1_phys_0_eng_0_engtype_3D", "pid_1_luid_0x0_0x1_phys_0"} {
		if _, err := parseGPUInstance(bad, true); err == nil {
			t.Fatal(bad)
		}
	}
}

func TestGPUBusiestEngineAcrossAdaptersIsNotASum(t *testing.T) {
	name1 := "pid_123_luid_0x0_0x1_phys_0_eng_0_engtype_3D"
	name2 := "pid_123_luid_0x0_0x2_phys_1_eng_3_engtype_Copy"
	engines := counterValues{Values: map[string]uint64{name1: math.Float64bits(25), name2: math.Float64bits(60)}}
	memory := counterValues{Values: map[string]uint64{"pid_123_luid_0x0_0x1_phys_0": 9007199254741000}}
	now := time.Now()
	p := assembleProcessGPUs(engines, memory, memory, now, now.Add(-time.Second))[123]
	if p.Percent == nil || *p.Percent != 60 || p.BusiestEngine.AdapterLUID != "00000000:00000002" || len(p.Engines) != 2 || *p.Memory[0].DedicatedBytes != 9007199254741000 {
		t.Fatalf("%+v", p)
	}
	warm := assembleProcessGPUs(engines, memory, memory, now, time.Time{})[123]
	if warm.Status != WarmingUp || warm.Percent != nil || len(warm.Engines) != 0 {
		t.Fatalf("%+v", warm)
	}
}
