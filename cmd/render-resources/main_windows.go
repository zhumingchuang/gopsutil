// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64 && cgo

// Package main is the modern, versioned monitor DLL. No legacy DLL exports or
// launcher/runtime entry points are imported here.
package main

/*
#include <stdint.h>
*/
import "C"

import (
	"context"
	"regexp"
	"sync"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v4/internal/resourcecollector"
)

var monitor struct {
	sync.Mutex
	collector *resourcecollector.Collector
	cancel    context.CancelFunc
	done      chan struct{}
}
var results resourcecollector.ResultStore
var validNodeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

//export RR_ABIVersion
func RR_ABIVersion() C.uint32_t { return 1 }

//export RR_StartProcessMonitor
func RR_StartProcessMonitor(node *C.char, length C.uint32_t) C.int {
	if node == nil || length == 0 || length > 128 {
		return resourcecollector.ABIInvalidArgument
	}
	id := C.GoStringN(node, C.int(length))
	if !validNodeID.MatchString(id) {
		return resourcecollector.ABIInvalidArgument
	}
	monitor.Lock()
	defer monitor.Unlock()
	if monitor.collector != nil {
		return resourcecollector.ABIAlreadyRunning
	}
	source, err := resourcecollector.NewWindowsSource()
	if err != nil {
		return resourcecollector.ABIInternalError
	}
	collector, err := resourcecollector.New(id, source)
	if err != nil {
		_ = source.Close()
		return resourcecollector.ABIInternalError
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	monitor.collector = collector
	monitor.cancel = cancel
	monitor.done = done
	go func() { defer close(done); _ = collector.Run(ctx, 2*time.Second, nil) }()
	return resourcecollector.ABIOK
}

//export RR_StopProcessMonitor
func RR_StopProcessMonitor() C.int {
	monitor.Lock()
	defer monitor.Unlock()
	if monitor.collector == nil {
		return resourcecollector.ABIOK
	}
	monitor.cancel()
	<-monitor.done
	err := monitor.collector.Close()
	monitor.collector = nil
	monitor.cancel = nil
	monitor.done = nil
	results.Clear()
	return C.int(resourcecollector.ABIError(err))
}

func put(value any, resultID *C.uint64_t) C.int {
	if resultID == nil {
		return resourcecollector.ABIInvalidArgument
	}
	id, code := results.Put(value)
	*resultID = C.uint64_t(id)
	return C.int(code)
}

// Serialize query publication with start/stop. A query racing with stop must
// not repopulate the result store after that monitor generation was cleared.
func request(resultID *C.uint64_t, fn func(*resourcecollector.Collector) (any, error)) C.int {
	if resultID == nil {
		return resourcecollector.ABIInvalidArgument
	}
	*resultID = 0
	monitor.Lock()
	defer monitor.Unlock()
	if monitor.collector == nil {
		return resourcecollector.ABINotRunning
	}
	value, err := fn(monitor.collector)
	if err != nil {
		return C.int(resourcecollector.ABIError(err))
	}
	return put(value, resultID)
}

//export RR_GetProcessCapabilities
func RR_GetProcessCapabilities(resultID *C.uint64_t) C.int {
	return request(resultID, func(c *resourcecollector.Collector) (any, error) { return c.Capabilities(), nil })
}

//export RR_GetNodeUsage
func RR_GetNodeUsage(resultID *C.uint64_t) C.int {
	return request(resultID, func(c *resourcecollector.Collector) (any, error) { return c.GetNodeUsage() })
}

func decode[T any](input *C.char, length C.uint32_t) (T, error) {
	var value T
	if input == nil || length == 0 || length > 4096 {
		return value, resourcecollector.ErrInvalidQuery
	}
	b := C.GoBytes(unsafe.Pointer(input), C.int(length))
	return resourcecollector.DecodeQuery[T](b)
}

//export RR_ListProcessUsage
func RR_ListProcessUsage(input *C.char, length C.uint32_t, resultID *C.uint64_t) C.int {
	query, err := decode[resourcecollector.ProcessListQuery](input, length)
	if err != nil {
		return C.int(resourcecollector.ABIError(err))
	}
	return request(resultID, func(c *resourcecollector.Collector) (any, error) { return c.ListProcessUsage(query) })
}

//export RR_GetProcessUsage
func RR_GetProcessUsage(input *C.char, length C.uint32_t, resultID *C.uint64_t) C.int {
	query, err := decode[resourcecollector.ProcessSelector](input, length)
	if err != nil {
		return C.int(resourcecollector.ABIError(err))
	}
	return request(resultID, func(c *resourcecollector.Collector) (any, error) { return c.GetProcessUsage(query) })
}

//export RR_GetProcessGroupUsage
func RR_GetProcessGroupUsage(input *C.char, length C.uint32_t, resultID *C.uint64_t) C.int {
	query, err := decode[resourcecollector.ProcessGroupQuery](input, length)
	if err != nil {
		return C.int(resourcecollector.ABIError(err))
	}
	return request(resultID, func(c *resourcecollector.Collector) (any, error) { return c.GetProcessGroupUsage(query) })
}

//export RR_ReadResult
func RR_ReadResult(id C.uint64_t, buffer *C.char, capacity C.uint32_t, required *C.uint32_t) C.int {
	if required == nil {
		return resourcecollector.ABIInvalidArgument
	}
	if buffer == nil && capacity > 0 {
		return resourcecollector.ABIInvalidArgument
	}
	if capacity > resourcecollector.MaximumSnapshotBytes+1 {
		return resourcecollector.ABIInvalidArgument
	}
	var out []byte
	if buffer != nil && capacity > 0 {
		out = unsafe.Slice((*byte)(unsafe.Pointer(buffer)), int(capacity))
	}
	size, code := results.Read(uint64(id), out)
	*required = C.uint32_t(size)
	return C.int(code)
}

//export RR_ReleaseResult
func RR_ReleaseResult(id C.uint64_t) C.int { return C.int(results.Release(uint64(id))) }

func main() {}
