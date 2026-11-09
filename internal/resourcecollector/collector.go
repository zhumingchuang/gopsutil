// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	MaximumProcesses     = 5000
	MaximumSnapshotBytes = 8 * 1024 * 1024
	SnapshotTTL          = 15 * time.Second
)

type baseline struct {
	at           time.Time
	user, kernel uint64
	cores        uint32
	read, write  *uint64
}
type storedSnapshot struct {
	snapshot   ProcessSnapshot
	encoded    []byte
	observedAt time.Time
}

// Collector owns its Source and keeps only two frozen snapshots. Capture is
// serialized, while queries never invoke the OS or wait for a sample interval.
type Collector struct {
	source        Source
	nodeID, epoch string
	cursorKey     [32]byte
	mu            sync.RWMutex
	captureMu     sync.Mutex
	previous      map[string]baseline
	snapshots     []storedSnapshot
	sequence      uint64
	closed        bool
}

func New(nodeID string, source Source) (*Collector, error) {
	if nodeID == "" || len(nodeID) > 128 || source == nil {
		return nil, ErrInvalidQuery
	}
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return nil, err
	}
	c := &Collector{source: source, nodeID: nodeID, epoch: hex.EncodeToString(epoch[:]), previous: make(map[string]baseline)}
	if _, err := rand.Read(c.cursorKey[:]); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Collector) Capabilities() Capabilities { return c.source.Capabilities() }

func (c *Collector) Sample(ctx context.Context) (string, error) {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return "", ErrClosed
	}
	raw, err := c.source.Capture(ctx)
	if err != nil {
		return "", err
	}
	if raw.LogicalProcessors == 0 {
		return "", fmt.Errorf("CPU_COUNT_INVALID")
	}
	c.sequence++
	raw.Node.SchemaVersion = SchemaVersion
	raw.Node.CollectorEpoch = c.epoch
	raw.Node.Sequence = c.sequence
	id := fmt.Sprintf("%s-%d", c.epoch, c.sequence)
	s := ProcessSnapshot{Node: raw.Node, SchemaVersion: SchemaVersion, CollectorEpoch: c.epoch, SnapshotID: id, Sequence: c.sequence,
		StartedAt: raw.StartedAt.UTC(), CompletedAt: raw.CompletedAt.UTC(), LogicalProcessors: raw.LogicalProcessors,
		TotalEnumerated: raw.TotalEnumerated, EnumerationComplete: raw.EnumerationComplete, LimitReason: raw.LimitReason, Rows: make([]ProcessUsage, 0, len(raw.Processes))}
	next := make(map[string]baseline, len(raw.Processes))
	seen := make(map[uint32]bool)
	for _, p := range raw.Processes {
		if seen[p.PID] {
			return "", fmt.Errorf("PROCESS_ENUMERATION_CONFLICT")
		}
		seen[p.PID] = true
		if len(s.Rows) == MaximumProcesses {
			s.EnumerationComplete = false
			s.LimitReason = "PROCESS_COUNT_LIMIT"
			break
		}
		row := ProcessUsage{RowID: fmt.Sprintf("%s:%d", id, p.PID), PID: p.PID, ParentPID: p.ParentPID, ParentVerified: p.ParentVerified, Name: p.Name, Kind: "process", State: "running",
			Identity: p.Identity, Memory: p.Memory, GenericIO: p.GenericIO, GPU: p.GPU,
			Disk: p.Disk, Network: p.Network}
		if row.Disk.Source == "" {
			row.Disk.Metric = unavailable(Unsupported, "etw.disk", "SOURCE_UNAVAILABLE", p.At)
		}
		if row.Network.Source == "" {
			row.Network.Metric = unavailable(Unsupported, "etw.network", "SOURCE_UNAVAILABLE", p.At)
		}
		if p.PID == 0 {
			row.Kind = "idle"
		}
		if p.Identity.Status != OK && p.Identity.Status != PermissionDenied {
			row.State = "unknown"
		}
		row.CPU.Metric = p.Identity
		row.CPU.Source = "windows.GetProcessTimes"
		if p.CreationTime != 0 && p.Identity.Status == OK {
			// FILETIME is an absolute 100 ns birth time. Epoch scopes these keys
			// to this collector instance; no baseline survives a collector restart.
			hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d/%d", c.nodeID, c.epoch, p.PID, p.CreationTime)))
			key := base64.RawURLEncoding.EncodeToString(hash[:])
			row.ProcessKey = &key
			row.CreationTime100ns = ptr(p.CreationTime)
			row.CPU.UserTime100ns = ptr(p.UserTime)
			row.CPU.KernelTime100ns = ptr(p.KernelTime)
			row.CPU.Status = WarmingUp
			old, exists := c.previous[key]
			elapsed := p.At.Sub(old.at)
			if exists && old.cores == raw.LogicalProcessors && elapsed > 0 && elapsed <= 15*time.Second && p.UserTime >= old.user && p.KernelTime >= old.kernel {
				corePercent := (float64(p.UserTime-old.user) + float64(p.KernelTime-old.kernel)) / (float64(elapsed.Nanoseconds()) / 100) * 100
				percent := corePercent / float64(raw.LogicalProcessors)
				if finite(percent) && percent >= 0 && percent <= 100.01 {
					row.CPU.Status = OK
					row.CPU.Percent = ptr(math.Min(percent, 100))
					row.CPU.CorePercent = ptr(corePercent)
					row.CPU.IntervalMs = float64(elapsed) / float64(time.Millisecond)
				}
				calculateIORate(&row.GenericIO, old, p, elapsed)
			}
			next[key] = baseline{at: p.At, user: p.UserTime, kernel: p.KernelTime, cores: raw.LogicalProcessors, read: p.GenericIO.ReadBytesTotal, write: p.GenericIO.WriteBytesTotal}
			if row.Memory.PrivateWorkingSetBytes != nil && raw.TotalPhysicalBytes > 0 {
				row.Memory.Percent = ptr(float64(*row.Memory.PrivateWorkingSetBytes) / float64(raw.TotalPhysicalBytes) * 100)
			}
		}
		if row.GenericIO.Status == OK && (row.GenericIO.ReadBytesPerSecond == nil || row.GenericIO.WriteBytesPerSecond == nil) {
			row.GenericIO.Status = WarmingUp
			row.GenericIO.ErrorCode = "RATE_BASELINE_REQUIRED"
		}
		s.Rows = append(s.Rows, row)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	if len(encoded) > MaximumSnapshotBytes {
		s.EnumerationComplete = false
		s.LimitReason = "SNAPSHOT_BYTE_LIMIT"
		allRows := s.Rows
		low, high := 0, len(allRows)
		for low < high {
			middle := (low + high + 1) / 2
			s.Rows = allRows[:middle]
			b, e := json.Marshal(s)
			if e != nil {
				return "", e
			}
			if len(b) <= MaximumSnapshotBytes {
				low = middle
			} else {
				high = middle - 1
			}
		}
		s.Rows = allRows[:low]
		encoded, err = json.Marshal(s)
		if err != nil {
			return "", err
		}
		if len(encoded) > MaximumSnapshotBytes {
			return "", ErrResourceLimited
		}
	}
	var frozen ProcessSnapshot
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", ErrClosed
	}
	c.previous = next
	c.snapshots = append(c.snapshots, storedSnapshot{snapshot: frozen, encoded: encoded, observedAt: raw.CompletedAt})
	if len(c.snapshots) > 2 {
		c.snapshots = c.snapshots[len(c.snapshots)-2:]
	}
	return id, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func calculateIORate(io *ProcessIO, old baseline, p RawProcess, elapsed time.Duration) {
	if io.Status != OK || io.ReadBytesTotal == nil || io.WriteBytesTotal == nil || old.read == nil || old.write == nil {
		return
	}
	if *io.ReadBytesTotal < *old.read || *io.WriteBytesTotal < *old.write {
		return
	}
	io.ReadBytesPerSecond = ptr(float64(*io.ReadBytesTotal-*old.read) / elapsed.Seconds())
	io.WriteBytesPerSecond = ptr(float64(*io.WriteBytesTotal-*old.write) / elapsed.Seconds())
	io.IntervalMs = float64(elapsed) / float64(time.Millisecond)
}

func (c *Collector) snapshot(id string) (ProcessSnapshot, error) {
	return c.readSnapshot(id, true)
}

// A view is private and read-only. Public query results clone only their selected
// fields/pages, so a 5000-process walk does not decode the full snapshot per page.
func (c *Collector) readSnapshot(id string, deep bool) (ProcessSnapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return ProcessSnapshot{}, ErrClosed
	}
	if len(c.snapshots) == 0 {
		return ProcessSnapshot{}, ErrNotCollecting
	}
	if id == "" {
		id = c.snapshots[len(c.snapshots)-1].snapshot.SnapshotID
	}
	for _, stored := range c.snapshots {
		if stored.snapshot.SnapshotID == id {
			age := time.Since(stored.observedAt)
			if age > SnapshotTTL {
				return ProcessSnapshot{}, ErrSnapshotExpired
			}
			// Queries receive their own copy, including pointer/slice fields.
			copy := stored.snapshot
			if deep {
				if err := json.Unmarshal(stored.encoded, &copy); err != nil {
					return copy, err
				}
			}
			copy.ageMs = float64(max(age, 0)) / float64(time.Millisecond)
			return copy, nil
		}
	}
	return ProcessSnapshot{}, ErrSnapshotExpired
}

func (c *Collector) GetProcessUsage(selector ProcessSelector) (ProcessUsage, error) {
	if len(selector.ExpectedProcessKey) > 128 || len(selector.SnapshotID) > 128 {
		return ProcessUsage{}, ErrInvalidQuery
	}
	s, err := c.readSnapshot(selector.SnapshotID, false)
	if err != nil {
		return ProcessUsage{}, err
	}
	for _, row := range s.Rows {
		if row.PID == selector.PID {
			if selector.ExpectedProcessKey != "" && (row.ProcessKey == nil || *row.ProcessKey != selector.ExpectedProcessKey) {
				return ProcessUsage{}, ErrIdentityChanged
			}
			body, err := json.Marshal(row)
			if err != nil {
				return ProcessUsage{}, err
			}
			var copy ProcessUsage
			err = json.Unmarshal(body, &copy)
			return copy, err
		}
	}
	return ProcessUsage{}, ErrProcessNotFound
}

func (c *Collector) Close() error {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.snapshots = nil
	c.previous = nil
	c.mu.Unlock()
	return c.source.Close()
}
