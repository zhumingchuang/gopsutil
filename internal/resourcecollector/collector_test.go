// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	samples []RawSample
	index   int
	closed  bool
}

func (s *fakeSource) Capture(context.Context) (RawSample, error) {
	v := s.samples[s.index]
	s.index++
	return v, nil
}
func (*fakeSource) Capabilities() Capabilities { return Capabilities{SchemaVersion: 1} }
func (s *fakeSource) Close() error             { s.closed = true; return nil }

func rawProcess(pid, parent uint32, birth, user uint64, at time.Time) RawProcess {
	return RawProcess{PID: pid, ParentPID: parent, ParentVerified: true, Name: "render.exe", CreationTime: birth, UserTime: user, At: at,
		Identity: Metric{Status: OK, CollectedAt: at}, Memory: ProcessMemory{Metric: Metric{Status: OK}, PrivateWorkingSetBytes: ptr(uint64(64)), WorkingSetBytes: ptr(uint64(128))},
		GenericIO: ProcessIO{Metric: Metric{Status: OK}, ReadBytesTotal: ptr(uint64(9007199254741000)), WriteBytesTotal: ptr(uint64(20))}}
}
func rawSample(at time.Time, cores uint32, processes ...RawProcess) RawSample {
	return RawSample{StartedAt: at, CompletedAt: at, LogicalProcessors: cores, TotalPhysicalBytes: 1024, TotalEnumerated: len(processes), EnumerationComplete: true, Processes: processes}
}
func newFake(t *testing.T, samples ...RawSample) *Collector {
	t.Helper()
	c, err := New("node-a", &fakeSource{samples: samples})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func sample(t *testing.T, c *Collector) string {
	t.Helper()
	id, err := c.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCPUBatchedWindowAndHostNormalization(t *testing.T) {
	now := time.Now()
	p1 := rawProcess(1, 0, 10, 0, now.Add(-time.Second))
	p2 := rawProcess(1, 0, 10, 10000000, now)
	c := newFake(t, rawSample(p1.At, 128, p1), rawSample(p2.At, 128, p2))
	sample(t, c)
	first, err := c.GetProcessUsage(ProcessSelector{PID: 1})
	if err != nil || first.CPU.Status != WarmingUp || first.CPU.Percent != nil {
		t.Fatalf("first=%+v err=%v", first.CPU, err)
	}
	sample(t, c)
	second, _ := c.GetProcessUsage(ProcessSelector{PID: 1})
	if second.CPU.Status != OK || *second.CPU.CorePercent != 100 || *second.CPU.Percent != 100.0/128 {
		t.Fatalf("cpu=%+v", second.CPU)
	}
	if second.Memory.Percent == nil || *second.Memory.Percent != 6.25 {
		t.Fatalf("memory=%+v", second.Memory)
	}
	if second.Disk.Status != Unsupported || second.Disk.ReadBytesPerSecond != nil || second.Network.RxBytesPerSecond != nil {
		t.Fatal("generic I/O leaked into disk/network")
	}
}

func TestTaskManagerDiskNetworkAndExactMemorySort(t *testing.T) {
	at := time.Now()
	a, b, unknown := rawProcess(1, 0, 10, 0, at), rawProcess(2, 0, 20, 0, at), rawProcess(3, 0, 30, 0, at)
	a.Disk = ProcessIO{Metric: Metric{Status: OK, Source: "test"}, ReadBytesPerSecond: ptr(10.0), WriteBytesPerSecond: ptr(10.0)}
	b.Disk = ProcessIO{Metric: a.Disk.Metric, ReadBytesPerSecond: ptr(20.0), WriteBytesPerSecond: ptr(5.0)}
	a.Network = ProcessNetwork{Metric: a.Disk.Metric, RxBytesPerSecond: ptr(100.0), TxBytesPerSecond: ptr(100.0)}
	b.Network = ProcessNetwork{Metric: a.Disk.Metric, RxBytesPerSecond: ptr(100.0), TxBytesPerSecond: ptr(0.0)}
	a.Memory.PrivateWorkingSetBytes = ptr(uint64(9007199254740992))
	b.Memory.PrivateWorkingSetBytes = ptr(uint64(9007199254740993))
	unknown.Memory.PrivateWorkingSetBytes = nil
	c := newFake(t, rawSample(at, 8, a, b, unknown))
	sample(t, c)
	for field, want := range map[string]uint32{"disk": 2, "network": 1, "memory": 2} {
		page, err := c.ListProcessUsage(ProcessListQuery{SortBy: field, Descending: true})
		if err != nil || len(page.Rows) != 3 || page.Rows[0].PID != want || page.Rows[2].PID != 3 {
			t.Fatalf("%s: %+v %v", field, page, err)
		}
	}
}

func TestPIDReuseCannotReuseCPUOrPinnedIdentity(t *testing.T) {
	now := time.Now()
	c := newFake(t, rawSample(now.Add(-time.Second), 8, rawProcess(42, 1, 10, 100, now.Add(-time.Second))), rawSample(now, 8, rawProcess(42, 1, 11, 200, now)))
	sample(t, c)
	first, _ := c.GetProcessUsage(ProcessSelector{PID: 42})
	sample(t, c)
	if _, err := c.GetProcessUsage(ProcessSelector{PID: 42, ExpectedProcessKey: *first.ProcessKey}); !errors.Is(err, ErrIdentityChanged) {
		t.Fatal(err)
	}
	second, _ := c.GetProcessUsage(ProcessSelector{PID: 42})
	if second.CPU.Status != WarmingUp || second.CPU.Percent != nil {
		t.Fatal("reused PID inherited CPU baseline")
	}
}

func TestCPUCounterResetOrTopologyChangeWarmsUp(t *testing.T) {
	for _, test := range []struct {
		name  string
		cores uint32
		time  uint64
	}{{"reset", 8, 99}, {"topology", 4, 200}} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			c := newFake(t, rawSample(now.Add(-time.Second), 8, rawProcess(1, 0, 1, 100, now.Add(-time.Second))), rawSample(now, test.cores, rawProcess(1, 0, 1, test.time, now)))
			sample(t, c)
			sample(t, c)
			p, _ := c.GetProcessUsage(ProcessSelector{PID: 1})
			if p.CPU.Status != WarmingUp || p.CPU.Percent != nil {
				t.Fatal(p.CPU)
			}
		})
	}
}

func TestProtectedProcessRetainedAndByteCountersAreDecimalStrings(t *testing.T) {
	now := time.Now()
	protected := rawProcess(2, 0, 0, 0, now)
	protected.Identity = Metric{Status: PermissionDenied, ErrorCode: "PROCESS_ACCESS_DENIED"}
	protected.Memory = ProcessMemory{Metric: protected.Identity}
	c := newFake(t, rawSample(now, 8, rawProcess(1, 0, 10, 0, now), protected))
	sample(t, c)
	page, err := c.ListProcessUsage(ProcessListQuery{})
	if err != nil || page.TotalEnumerated != 2 || len(page.Rows) != 2 || page.Rows[1].ProcessKey != nil || page.Rows[1].CPU.Percent != nil {
		t.Fatalf("%+v %v", page, err)
	}
	b, _ := json.Marshal(page)
	if !strings.Contains(string(b), `"readBytesTotal":"9007199254741000"`) {
		t.Fatal(string(b))
	}
}

func TestFrozenPaginationAndCursorBinding(t *testing.T) {
	now := time.Now()
	c := newFake(t, rawSample(now, 8, rawProcess(1, 0, 1, 0, now), rawProcess(2, 1, 2, 0, now), rawProcess(3, 1, 3, 0, now)), rawSample(now, 8, rawProcess(4, 0, 4, 0, now)))
	id := sample(t, c)
	first, err := c.ListProcessUsage(ProcessListQuery{Limit: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("%+v %v", first, err)
	}
	sample(t, c)
	second, err := c.ListProcessUsage(ProcessListQuery{Limit: 2, Cursor: first.NextCursor})
	if err != nil || second.SnapshotID != id || len(second.Rows) != 1 || second.Rows[0].PID != 3 {
		t.Fatalf("%+v %v", second, err)
	}
	if _, err = c.ListProcessUsage(ProcessListQuery{Limit: 2, Cursor: first.NextCursor, Name: "different"}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal(err)
	}
	b, _ := base64.RawURLEncoding.DecodeString(first.NextCursor)
	var cursor pageCursor
	_ = json.Unmarshal(b, &cursor)
	cursor.Offset = 1
	b, _ = json.Marshal(cursor)
	if _, err = c.ListProcessUsage(ProcessListQuery{Limit: 2, Cursor: base64.RawURLEncoding.EncodeToString(b)}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("tampered cursor accepted", err)
	}
}

func TestQueriesCannotMutateStoredSnapshot(t *testing.T) {
	now := time.Now()
	c := newFake(t, rawSample(now, 8, rawProcess(1, 0, 1, 0, now)))
	sample(t, c)
	p, _ := c.GetProcessUsage(ProcessSelector{PID: 1})
	*p.Memory.PrivateWorkingSetBytes = 999
	page, _ := c.ListProcessUsage(ProcessListQuery{})
	if *page.Rows[0].Memory.PrivateWorkingSetBytes != 64 {
		t.Fatal("query mutated frozen snapshot")
	}
}

func TestSnapshotExpiryAndBoundedRetention(t *testing.T) {
	now := time.Now()
	c := newFake(t, rawSample(now, 8), rawSample(now, 8), rawSample(now, 8))
	first := sample(t, c)
	sample(t, c)
	sample(t, c)
	if _, err := c.ListProcessUsage(ProcessListQuery{SnapshotID: first}); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatal(err)
	}
	expired := newFake(t, rawSample(now.Add(-20*time.Second), 8))
	sample(t, expired)
	if _, err := expired.ListProcessUsage(ProcessListQuery{}); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatal(err)
	}
}

func TestTreeDoesNotClaimChildrenOfRecycledParentPID(t *testing.T) {
	now := time.Now()
	c := newFake(t, rawSample(now, 8, rawProcess(10, 1, 100, 0, now), rawProcess(11, 10, 200, 0, now), rawProcess(12, 10, 50, 0, now)))
	sample(t, c)
	root, _ := c.GetProcessUsage(ProcessSelector{PID: 10})
	group, err := c.GetProcessGroupUsage(ProcessGroupQuery{RootProcessKey: *root.ProcessKey})
	if err != nil || group.MemberCount != 2 || group.PrivateWorkingSetBytes == nil || *group.PrivateWorkingSetBytes != 128 || group.CPUStatus != Partial {
		t.Fatalf("%+v %v", group, err)
	}
}

func TestUnknownValuesSortLastInEitherDirection(t *testing.T) {
	now := time.Now()
	a, b := rawProcess(1, 0, 1, 0, now), rawProcess(2, 0, 2, 0, now)
	a.Memory.PrivateWorkingSetBytes = nil
	c := newFake(t, rawSample(now, 8, a, b))
	sample(t, c)
	for _, desc := range []bool{true, false} {
		p, e := c.ListProcessUsage(ProcessListQuery{SortBy: "memory", Descending: desc})
		if e != nil || p.Rows[0].PID != 2 {
			t.Fatalf("%+v %v", p, e)
		}
	}
}

func TestCloseAndCancellation(t *testing.T) {
	now := time.Now()
	source := &fakeSource{samples: []RawSample{rawSample(now, 8)}}
	c, _ := New("node-a", source)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Run(ctx, time.Second, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = c.Close()
	if !source.closed {
		t.Fatal("source not closed")
	}
	if _, err := c.Sample(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestFiniteRejectsNaNAndInfinity(t *testing.T) {
	if finite(math.NaN()) || finite(math.Inf(1)) {
		t.Fatal("nonfinite accepted")
	}
}

func TestByteBoundedPaginationStillReturnsEveryRow(t *testing.T) {
	now := time.Now()
	processes := make([]RawProcess, 200)
	for i := range processes {
		processes[i] = rawProcess(uint32(i+1), 0, uint64(i+1), 0, now)
		processes[i].Name = strings.Repeat("x", 200)
	}
	c := newFake(t, rawSample(now, 8, processes...))
	sample(t, c)
	query := ProcessListQuery{Limit: 500}
	seen := make(map[uint32]bool)
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatal("pagination did not terminate")
		}
		page, err := c.ListProcessUsage(query)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(page)
		if len(body) > 64*1024 {
			t.Fatal("page exceeds byte limit")
		}
		for _, row := range page.Rows {
			if seen[row.PID] {
				t.Fatal("duplicate PID")
			}
			seen[row.PID] = true
		}
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if len(seen) != 200 {
		t.Fatal("incomplete pagination", len(seen))
	}
}

func TestOversizedSnapshotReportsIncompleteEnumeration(t *testing.T) {
	now := time.Now()
	processes := make([]RawProcess, 300)
	for i := range processes {
		processes[i] = rawProcess(uint32(i+1), 0, uint64(i+1), 0, now)
		processes[i].Name = strings.Repeat("x", 32768)
	}
	c := newFake(t, rawSample(now, 8, processes...))
	id := sample(t, c)
	snapshot, err := c.snapshot(id)
	if err != nil || snapshot.EnumerationComplete || snapshot.TotalEnumerated != 300 || len(snapshot.Rows) >= 300 || snapshot.LimitReason != "SNAPSHOT_BYTE_LIMIT" {
		t.Fatalf("rows=%d complete=%v reason=%s err=%v", len(snapshot.Rows), snapshot.EnumerationComplete, snapshot.LimitReason, err)
	}
}

func TestConcurrentSamplingAndQueriesKeepSnapshotsCoherent(t *testing.T) {
	now := time.Now()
	samples := make([]RawSample, 31)
	for i := range samples {
		samples[i] = rawSample(now, 8, rawProcess(42, 1, 10, uint64(i), now))
	}
	c := newFake(t, samples...)
	sample(t, c)
	var workers sync.WaitGroup
	failures := make(chan error, 431)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := 0; i < 30; i++ {
			if _, err := c.Sample(context.Background()); err != nil {
				failures <- err
			}
		}
	}()
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				page, err := c.ListProcessUsage(ProcessListQuery{})
				if err != nil {
					failures <- err
					continue
				}
				if len(page.Rows) != 1 || page.Rows[0].PID != 42 || page.Rows[0].ProcessKey == nil {
					failures <- errors.New("incoherent snapshot")
				}
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func TestUnverifiedParentCannotCreateACompleteTreeAggregate(t *testing.T) {
	now := time.Now()
	child := rawProcess(11, 10, 200, 0, now)
	child.ParentVerified = false
	c := newFake(t, rawSample(now, 8, rawProcess(10, 1, 100, 0, now), child))
	sample(t, c)
	root, _ := c.GetProcessUsage(ProcessSelector{PID: 10})
	group, err := c.GetProcessGroupUsage(ProcessGroupQuery{RootProcessKey: *root.ProcessKey})
	if err != nil || group.MemberCount != 1 || group.MembershipComplete || group.PrivateWorkingSetBytes != nil || group.MemoryStatus != Partial {
		t.Fatalf("%+v %v", group, err)
	}
}
