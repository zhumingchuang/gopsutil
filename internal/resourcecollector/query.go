// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type pageCursor struct {
	SnapshotID string `json:"snapshotId"`
	Offset     int    `json:"offset"`
	QueryHash  string `json:"queryHash"`
	Signature  string `json:"signature"`
}

func (c *Collector) signCursor(cursor pageCursor) string {
	cursor.Signature = ""
	b, _ := json.Marshal(cursor)
	h := hmac.New(sha256.New, c.cursorKey[:])
	_, _ = h.Write(b)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (c *Collector) ListProcessUsage(query ProcessListQuery) (ProcessPage, error) {
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Limit < 1 || query.Limit > 500 || len(query.Name) > 256 || len(query.Cursor) > 2048 || len(query.SnapshotID) > 128 {
		return ProcessPage{}, ErrInvalidQuery
	}
	if query.SortBy == "" {
		query.SortBy = "pid"
	}
	switch query.SortBy {
	case "pid", "name", "cpu", "memory", "gpu", "disk", "network":
	default:
		return ProcessPage{}, ErrInvalidQuery
	}
	identity := query
	identity.Cursor = ""
	identity.SnapshotID = ""
	encoded, _ := json.Marshal(identity)
	hash := sha256.Sum256(encoded)
	queryHash := base64.RawURLEncoding.EncodeToString(hash[:])
	offset := 0
	if query.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil {
			return ProcessPage{}, ErrInvalidQuery
		}
		var cursor pageCursor
		if json.Unmarshal(b, &cursor) != nil || cursor.Offset < 0 || cursor.QueryHash != queryHash || !hmac.Equal([]byte(cursor.Signature), []byte(c.signCursor(cursor))) {
			return ProcessPage{}, ErrInvalidQuery
		}
		if query.SnapshotID != "" && query.SnapshotID != cursor.SnapshotID {
			return ProcessPage{}, ErrInvalidQuery
		}
		query.SnapshotID = cursor.SnapshotID
		offset = cursor.Offset
	}
	s, err := c.readSnapshot(query.SnapshotID, false)
	if err != nil {
		return ProcessPage{}, err
	}
	rows := make([]ProcessUsage, 0, len(s.Rows))
	for _, row := range s.Rows {
		if query.PID != nil && row.PID != *query.PID {
			continue
		}
		if !strings.Contains(strings.ToLower(row.Name), strings.ToLower(query.Name)) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if query.SortBy == "memory" {
			av, bv := a.Memory.PrivateWorkingSetBytes, b.Memory.PrivateWorkingSetBytes
			if av == nil && bv != nil {
				return false
			}
			if av != nil && bv == nil {
				return true
			}
			if av != nil && bv != nil && *av != *bv {
				if query.Descending {
					return *av > *bv
				}
				return *av < *bv
			}
		} else if query.SortBy == "cpu" || query.SortBy == "gpu" || query.SortBy == "disk" || query.SortBy == "network" {
			av, bv := sortValue(a, query.SortBy), sortValue(b, query.SortBy)
			if av == nil && bv != nil {
				return false
			}
			if av != nil && bv == nil {
				return true
			}
			if av != nil && bv != nil && *av != *bv {
				if query.Descending {
					return *av > *bv
				}
				return *av < *bv
			}
		} else if query.SortBy == "name" && a.Name != b.Name {
			if query.Descending {
				return a.Name > b.Name
			}
			return a.Name < b.Name
		}
		if query.SortBy == "pid" && query.Descending {
			return a.PID > b.PID
		}
		return a.PID < b.PID
	})
	if offset > len(rows) {
		return ProcessPage{}, ErrInvalidQuery
	}
	end := min(offset+query.Limit, len(rows))
	page := ProcessPage{SchemaVersion: SchemaVersion, CollectorEpoch: s.CollectorEpoch, SnapshotID: s.SnapshotID, Sequence: s.Sequence, StartedAt: s.StartedAt, CompletedAt: s.CompletedAt,
		AgeMs: s.ageMs, EnumerationComplete: s.EnumerationComplete, TotalEnumerated: s.TotalEnumerated, TotalMatched: len(rows), LimitReason: s.LimitReason, Rows: rows[offset:end]}
	for {
		page.Rows = rows[offset:end]
		page.NextCursor = ""
		if end < len(rows) {
			cursor := pageCursor{SnapshotID: s.SnapshotID, Offset: end, QueryHash: queryHash}
			cursor.Signature = c.signCursor(cursor)
			b, _ := json.Marshal(cursor)
			page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
		}
		body, err := json.Marshal(page)
		if err != nil {
			return ProcessPage{}, err
		}
		if len(body) <= 64*1024 {
			var copy ProcessPage
			err = json.Unmarshal(body, &copy)
			return copy, err
		}
		if end <= offset+1 {
			return ProcessPage{}, ErrResourceLimited
		}
		end = offset + (end-offset)/2
	}
}

func sortValue(p ProcessUsage, key string) *float64 {
	switch key {
	case "cpu":
		return p.CPU.Percent
	case "gpu":
		return p.GPU.Percent
	case "disk":
		if p.Disk.ReadBytesPerSecond != nil && p.Disk.WriteBytesPerSecond != nil {
			return ptr(*p.Disk.ReadBytesPerSecond + *p.Disk.WriteBytesPerSecond)
		}
	case "network":
		if p.Network.RxBytesPerSecond != nil && p.Network.TxBytesPerSecond != nil {
			return ptr(*p.Network.RxBytesPerSecond + *p.Network.TxBytesPerSecond)
		}
	case "memory":
		if p.Memory.PrivateWorkingSetBytes != nil {
			return ptr(float64(*p.Memory.PrivateWorkingSetBytes))
		}
	}
	return nil
}

func (c *Collector) GetProcessGroupUsage(query ProcessGroupQuery) (ProcessGroup, error) {
	if query.RootProcessKey == "" || len(query.RootProcessKey) > 128 || len(query.SnapshotID) > 128 {
		return ProcessGroup{}, ErrInvalidQuery
	}
	s, err := c.snapshot(query.SnapshotID)
	if err != nil {
		return ProcessGroup{}, err
	}
	var root *ProcessUsage
	for i := range s.Rows {
		if s.Rows[i].ProcessKey != nil && *s.Rows[i].ProcessKey == query.RootProcessKey {
			root = &s.Rows[i]
			break
		}
	}
	if root == nil {
		return ProcessGroup{}, ErrProcessNotFound
	}
	// A parent must have been born before the child. This prevents a recycled
	// parent PID from claiming an older child in a current-process snapshot.
	members := map[uint32]ProcessUsage{root.PID: *root}
	for changed := true; changed; {
		changed = false
		for _, row := range s.Rows {
			if row.ProcessKey == nil || row.CreationTime100ns == nil || !row.ParentVerified {
				continue
			}
			if _, ok := members[row.PID]; ok {
				continue
			}
			if parent, ok := members[row.ParentPID]; ok && parent.CreationTime100ns != nil && *parent.CreationTime100ns <= *row.CreationTime100ns {
				members[row.PID] = row
				changed = true
			}
		}
	}
	group := ProcessGroup{SnapshotID: s.SnapshotID, RootProcessKey: query.RootProcessKey, Kind: "tree", Members: []string{}, CPUStatus: OK, MemoryStatus: OK,
		GPU: ProcessGPU{Metric: unavailable(Partial, "windows.pdh.gpu.group", "GPU_MEMBERS_UNAVAILABLE", s.CompletedAt), Engines: []GPUEngine{}, Memory: []GPUMemory{}}}
	group.MembershipComplete = s.EnumerationComplete
	for _, row := range s.Rows {
		if _, parentMember := members[row.ParentPID]; parentMember && (row.ProcessKey == nil || row.CreationTime100ns == nil || !row.ParentVerified) {
			group.MembershipComplete = false
		}
	}
	var cpu float64
	var memory uint64
	for _, row := range members {
		group.Members = append(group.Members, *row.ProcessKey)
		if row.CPU.Status != OK || row.CPU.Percent == nil || row.Kind == "idle" {
			group.CPUStatus = Partial
		} else {
			cpu += *row.CPU.Percent
		}
		if row.Memory.PrivateWorkingSetBytes == nil {
			group.MemoryStatus = Partial
		} else {
			memory += *row.Memory.PrivateWorkingSetBytes
		}
	}
	group.MemberCount = len(members)
	aggregateGroupResources(&group, members, s.CompletedAt)
	sort.Strings(group.Members)
	if group.CPUStatus == OK && group.MembershipComplete {
		group.CPUPercent = ptr(cpu)
	} else {
		group.CPUStatus = Partial
	}
	if group.MemoryStatus == OK && group.MembershipComplete {
		group.PrivateWorkingSetBytes = ptr(memory)
	} else {
		group.MemoryStatus = Partial
	}
	return group, nil
}

// Run samples immediately, then waits between completed captures. A slow native
// call cannot create an unbounded queue of overlapping capture goroutines.
func (c *Collector) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval < time.Second {
		return ErrInvalidQuery
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := c.Sample(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if report != nil {
				report(err)
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
