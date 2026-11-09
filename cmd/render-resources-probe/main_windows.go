// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64

// This developer probe reads local counters only. It never opens a listener,
// starts a renderer, elevates privileges or changes a running product.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/shirou/gopsutil/v4/internal/resourcecollector"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	source, err := resourcecollector.NewWindowsSource()
	if err != nil {
		return err
	}
	defer source.Close()
	collector, err := resourcecollector.New("local-probe", source)
	if err != nil {
		return err
	}
	defer collector.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err = collector.Sample(ctx); err != nil {
		return err
	}
	time.Sleep(time.Second)
	if _, err = collector.Sample(ctx); err != nil {
		return err
	}
	page, err := collector.ListProcessUsage(resourcecollector.ProcessListQuery{Limit: 500, SortBy: "gpu", Descending: true})
	if err != nil {
		return err
	}
	self, err := collector.GetProcessUsage(resourcecollector.ProcessSelector{PID: uint32(os.Getpid())})
	if err != nil {
		return err
	}
	allRows := append([]resourcecollector.ProcessUsage{}, page.Rows...)
	for cursor := page.NextCursor; cursor != ""; {
		next, e := collector.ListProcessUsage(resourcecollector.ProcessListQuery{Limit: 500, SortBy: "gpu", Descending: true, Cursor: cursor})
		if e != nil {
			return e
		}
		allRows = append(allRows, next.Rows...)
		cursor = next.NextCursor
	}
	var cpu, memory, gpu int
	for _, row := range allRows {
		if row.CPU.Percent != nil {
			cpu++
		}
		if row.Memory.PrivateWorkingSetBytes != nil {
			memory++
		}
		if row.GPU.Percent != nil {
			gpu++
		}
	}
	result := struct {
		Capabilities resourcecollector.Capabilities   `json:"capabilities"`
		Total        int                              `json:"totalEnumerated"`
		Returned     int                              `json:"returned"`
		CPU          int                              `json:"cpuReadable"`
		Memory       int                              `json:"privateWorkingSetReadable"`
		GPU          int                              `json:"gpuReadable"`
		Next         bool                             `json:"hasNextPage"`
		Self         resourcecollector.ProcessUsage   `json:"self"`
		GPUExamples  []resourcecollector.ProcessUsage `json:"gpuExamples"`
	}{collector.Capabilities(), page.TotalEnumerated, len(allRows), cpu, memory, gpu, false, self, []resourcecollector.ProcessUsage{}}
	for _, row := range allRows {
		if row.GPU.Percent != nil && row.PID != 0 {
			result.GPUExamples = append(result.GPUExamples, row)
			if len(result.GPUExamples) == 2 {
				break
			}
		}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
