// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotCollecting   = errors.New("NOT_COLLECTING")
	ErrSnapshotExpired = errors.New("SNAPSHOT_EXPIRED")
	ErrProcessNotFound = errors.New("PROCESS_NOT_FOUND")
	ErrIdentityChanged = errors.New("PROCESS_IDENTITY_CHANGED")
	ErrInvalidQuery    = errors.New("PROCESS_QUERY_INVALID")
	ErrClosed          = errors.New("COLLECTOR_CLOSED")
	ErrResourceLimited = errors.New("RESOURCE_LIMITED")
)

// RawProcess is read from one OS process handle. Identity and CPU times must
// refer to that same handle; a PID alone is never a rate-calculation key.
type RawProcess struct {
	PID, ParentPID                     uint32
	ParentVerified                     bool
	Name                               string
	CreationTime, UserTime, KernelTime uint64
	At                                 time.Time
	Identity                           Metric
	Memory                             ProcessMemory
	GenericIO                          ProcessIO
	GPU                                ProcessGPU
	Disk                               ProcessIO
	Network                            ProcessNetwork
}

type RawSample struct {
	Node                   NodeSnapshot
	StartedAt, CompletedAt time.Time
	LogicalProcessors      uint32
	TotalPhysicalBytes     uint64
	TotalEnumerated        int
	EnumerationComplete    bool
	LimitReason            string
	Processes              []RawProcess
}

type Source interface {
	Capture(context.Context) (RawSample, error)
	Capabilities() Capabilities
	Close() error
}

func ptr[T any](v T) *T { return &v }

func unavailable(status Status, source, code string, at time.Time) Metric {
	return Metric{Status: status, Source: source, ErrorCode: code, CollectedAt: at}
}
