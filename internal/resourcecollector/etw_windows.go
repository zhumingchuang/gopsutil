// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64 && cgo

package resourcecollector

/*
#cgo LDFLAGS: -ladvapi32 -ltdh
#include "etw_windows.h"
*/
import "C"
import (
	"fmt"
	"time"
)

type ioIdentity struct {
	pid   uint32
	birth uint64
}
type ioTotals struct{ read, write, rx, tx uint64 }
type etwSource struct {
	handle        *C.rr_trace
	status        uint32
	at            time.Time
	totals        map[ioIdentity]ioTotals
	lost, unknown uint64
	tainted       bool
}

func newETWSource() *etwSource {
	var status C.uint32_t
	h := C.rr_trace_start(&status)
	return &etwSource{handle: h, status: uint32(status), at: time.Now(), totals: map[ioIdentity]ioTotals{}}
}
func (e *etwSource) capability(source string) Capability {
	v := Capability{Supported: e.handle != nil, Source: source}
	if !v.Supported {
		v.Reason = fmt.Sprintf("ETW_START_%d", e.status)
		if e.status == 5 || e.status == 1314 {
			v.Reason = "ETW_PERMISSION_DENIED"
		}
		if e.status == 183 {
			v.Reason = "ETW_SESSION_ALREADY_EXISTS"
		}
	}
	return v
}

// ETW converts QPC to FILETIME using its session origin. Recreate only our own
// session after a wall-clock step so a recycled PID cannot join an old event.
func (e *etwSource) clockCheck() {
	now := time.Now()
	if !e.at.IsZero() {
		difference := now.UTC().Sub(e.at.UTC()) - now.Sub(e.at)
		if difference > time.Millisecond || difference < -time.Millisecond {
			e.close()
			next := newETWSource()
			next.tainted = true
			*e = *next
		}
	}
}
func (e *etwSource) capture(rows []RawProcess) {
	at := time.Now()
	elapsed := at.Sub(e.at)
	metric := nodeMetric("windows.etw", at)
	metric.IntervalMs = float64(elapsed) / float64(time.Millisecond)
	if e.handle == nil {
		metric = unavailable(Failed, "windows.etw", e.capability("").Reason, at)
		if e.status == 5 || e.status == 1314 {
			metric.Status = PermissionDenied
		}
		for i := range rows {
			rows[i].Disk = ProcessIO{Metric: metric}
			rows[i].Network = ProcessNetwork{Metric: metric}
		}
		return
	}
	events := make([]C.rr_io_event, 65536)
	var lost, unknown C.uint64_t
	var health C.uint32_t
	count := C.rr_trace_drain(e.handle, &events[0], C.uint32_t(len(events)), &lost, &unknown, &health)
	if health != 0 {
		for i := range rows {
			m := unavailable(Failed, "windows.etw", fmt.Sprintf("ETW_CONSUMER_%d", health), at)
			rows[i].Disk = ProcessIO{Metric: m}
			rows[i].Network = ProcessNetwork{Metric: m}
		}
		e.totals = map[ioIdentity]ioTotals{}
		e.at = at
		return
	}
	deltas := map[ioIdentity]ioTotals{}
	for _, event := range events[:int(count)] {
		key := ioIdentity{uint32(event.pid), uint64(event.birth)}
		v := deltas[key]
		b := uint64(event.bytes)
		switch event.kind {
		case 1:
			v.read += b
		case 2:
			v.write += b
		case 3:
			v.rx += b
		case 4:
			v.tx += b
		}
		deltas[key] = v
	}
	e.apply(rows, deltas, at, uint64(lost), uint64(unknown))
}
func (e *etwSource) apply(rows []RawProcess, deltas map[ioIdentity]ioTotals, at time.Time, lost, unknown uint64) {
	elapsed := at.Sub(e.at)
	metric := nodeMetric("windows.etw", at)
	metric.IntervalMs = float64(elapsed) / float64(time.Millisecond)
	if lost != e.lost || unknown != e.unknown {
		e.tainted = true
		metric.Status = Partial
		metric.ErrorCode = "ETW_LOST_OR_UNATTRIBUTED_EVENTS"
	}
	if elapsed <= 0 || elapsed > SnapshotTTL {
		metric.Status = WarmingUp
		metric.ErrorCode = "ETW_WINDOW_RESET"
	}
	next := map[ioIdentity]ioTotals{}
	for i := range rows {
		p := &rows[i]
		key := ioIdentity{p.PID, p.CreationTime}
		old, known := e.totals[key]
		delta := deltas[key]
		v := ioTotals{old.read + delta.read, old.write + delta.write, old.rx + delta.rx, old.tx + delta.tx}
		next[key] = v
		m := metric
		if p.Identity.Status != OK {
			m = p.Identity
		} else if !known {
			m.Status = WarmingUp
			m.ErrorCode = "ETW_IDENTITY_WINDOW_WARMING_UP"
		}
		p.Disk = ProcessIO{Metric: m}
		p.Network = ProcessNetwork{Metric: m}
		if p.Identity.Status == OK {
			p.Disk.ReadBytesTotal = ptr(v.read)
			p.Disk.WriteBytesTotal = ptr(v.write)
			p.Network.RxBytesSinceEpoch = ptr(v.rx)
			p.Network.TxBytesSinceEpoch = ptr(v.tx)
		}
		if m.Status == OK {
			p.Disk.ReadBytesPerSecond = ptr(float64(delta.read) / elapsed.Seconds())
			p.Disk.WriteBytesPerSecond = ptr(float64(delta.write) / elapsed.Seconds())
			p.Network.RxBytesPerSecond = ptr(float64(delta.rx) / elapsed.Seconds())
			p.Network.TxBytesPerSecond = ptr(float64(delta.tx) / elapsed.Seconds())
		}
		if e.tainted {
			p.Disk.ReadBytesTotal = nil
			p.Disk.WriteBytesTotal = nil
			p.Network.RxBytesSinceEpoch = nil
			p.Network.TxBytesSinceEpoch = nil
			if m.Status == OK {
				p.Disk.Status = Partial
				p.Network.Status = Partial
				p.Disk.ErrorCode = "ETW_TOTALS_INCOMPLETE"
				p.Network.ErrorCode = "ETW_TOTALS_INCOMPLETE"
			}
		}
	}
	e.totals = next
	e.at = at
	e.lost = uint64(lost)
	e.unknown = uint64(unknown)
}
func (e *etwSource) close() {
	if e.handle != nil {
		C.rr_trace_stop(e.handle)
		e.handle = nil
	}
}
