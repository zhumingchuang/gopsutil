//go:build windows && amd64 && cgo

// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"testing"
	"time"
)

func TestETWBirthCorrelationLossAndCounterPrecision(t *testing.T) {
	at := time.Now()
	key := ioIdentity{42, 10}
	e := &etwSource{at: at.Add(-2 * time.Second), totals: map[ioIdentity]ioTotals{key: {read: 9007199254740993, tx: 10}}}
	rows := []RawProcess{{PID: 42, CreationTime: 10, Identity: Metric{Status: OK}}}
	e.apply(rows, map[ioIdentity]ioTotals{key: {read: 200, write: 40, rx: 60, tx: 80}}, at, 0, 0)
	if *rows[0].Disk.ReadBytesTotal != 9007199254741193 || *rows[0].Disk.ReadBytesPerSecond != 100 || *rows[0].Network.TxBytesPerSecond != 40 {
		t.Fatalf("%+v", rows[0])
	}
	rows[0].CreationTime = 11
	e.apply(rows, map[ioIdentity]ioTotals{key: {read: 10000}, {42, 11}: {read: 20}}, at.Add(2*time.Second), 0, 0)
	if rows[0].Disk.Status != WarmingUp || rows[0].Disk.ReadBytesPerSecond != nil || *rows[0].Disk.ReadBytesTotal != 20 {
		t.Fatal("recycled PID inherited ETW history")
	}
	e.apply(rows, map[ioIdentity]ioTotals{{42, 11}: {read: 10}}, at.Add(4*time.Second), 1, 0)
	if rows[0].Disk.Status != Partial || rows[0].Disk.ReadBytesPerSecond != nil {
		t.Fatal("lost events became trustworthy zero/rate")
	}
	e.apply(rows, nil, at.Add(6*time.Second), 1, 0)
	if rows[0].Network.Status != Partial || *rows[0].Network.RxBytesPerSecond != 0 || rows[0].Network.RxBytesSinceEpoch != nil {
		t.Fatal("healthy zero window not distinguished from loss")
	}
	e.apply(rows, nil, at.Add(30*time.Second), 1, 0)
	if rows[0].Network.Status != WarmingUp || rows[0].Network.RxBytesPerSecond != nil {
		t.Fatal("stale ETW window accepted")
	}
}
