// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import "time"

const SchemaVersion = 1

type Status string

const (
	OK               Status = "ok"
	WarmingUp        Status = "warming_up"
	Partial          Status = "partial"
	Unsupported      Status = "unsupported"
	PermissionDenied Status = "permission_denied"
	Failed           Status = "error"
)

type Metric struct {
	Status      Status    `json:"status"`
	Source      string    `json:"source"`
	ErrorCode   string    `json:"errorCode,omitempty"`
	CollectedAt time.Time `json:"collectedAt"`
	IntervalMs  float64   `json:"intervalMs"`
}

type ProcessCPU struct {
	Metric
	Percent         *float64 `json:"percent"`
	CorePercent     *float64 `json:"corePercent"`
	UserTime100ns   *uint64  `json:"userTime100ns,string"`
	KernelTime100ns *uint64  `json:"kernelTime100ns,string"`
}

type ProcessMemory struct {
	Metric
	WorkingSetBytes        *uint64  `json:"workingSetBytes,string"`
	PrivateWorkingSetBytes *uint64  `json:"privateWorkingSetBytes,string"`
	PrivateCommitBytes     *uint64  `json:"privateCommitBytes,string"`
	Percent                *float64 `json:"percent"`
}

type ProcessIO struct {
	Metric
	ReadBytesTotal      *uint64  `json:"readBytesTotal,string"`
	WriteBytesTotal     *uint64  `json:"writeBytesTotal,string"`
	ReadBytesPerSecond  *float64 `json:"readBytesPerSecond"`
	WriteBytesPerSecond *float64 `json:"writeBytesPerSecond"`
}

type ProcessNetwork struct {
	Metric
	RxBytesSinceEpoch *uint64  `json:"rxBytesSinceEpoch,string"`
	TxBytesSinceEpoch *uint64  `json:"txBytesSinceEpoch,string"`
	RxBytesPerSecond  *float64 `json:"rxBytesPerSecond"`
	TxBytesPerSecond  *float64 `json:"txBytesPerSecond"`
}

type GPUEngine struct {
	AdapterLUID          string  `json:"adapterLuid"`
	PhysicalAdapterIndex uint32  `json:"physicalAdapterIndex"`
	EngineID             uint32  `json:"engineId"`
	EngineType           string  `json:"engineType"`
	Percent              float64 `json:"percent"`
}

type GPUMemory struct {
	AdapterLUID          string  `json:"adapterLuid"`
	PhysicalAdapterIndex uint32  `json:"physicalAdapterIndex"`
	DedicatedBytes       *uint64 `json:"dedicatedBytes,string"`
	SharedBytes          *uint64 `json:"sharedBytes,string"`
}

type ProcessGPU struct {
	Metric
	Percent       *float64    `json:"percent"`
	BusiestEngine *GPUEngine  `json:"busiestEngine"`
	Engines       []GPUEngine `json:"engines"`
	Memory        []GPUMemory `json:"memory"`
}

type ProcessUsage struct {
	RowID             string         `json:"rowId"`
	ProcessKey        *string        `json:"processKey"`
	PID               uint32         `json:"pid"`
	ParentPID         uint32         `json:"parentPid"`
	ParentVerified    bool           `json:"parentVerified"`
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	State             string         `json:"state"`
	CreationTime100ns *uint64        `json:"creationTime100ns,string"`
	Identity          Metric         `json:"identity"`
	CPU               ProcessCPU     `json:"cpu"`
	Memory            ProcessMemory  `json:"memory"`
	GenericIO         ProcessIO      `json:"genericIo"`
	Disk              ProcessIO      `json:"disk"`
	Network           ProcessNetwork `json:"network"`
	GPU               ProcessGPU     `json:"gpu"`
}

type ProcessSnapshot struct {
	Node                NodeSnapshot `json:"node"`
	ageMs               float64
	SchemaVersion       int            `json:"schemaVersion"`
	CollectorEpoch      string         `json:"collectorEpoch"`
	SnapshotID          string         `json:"snapshotId"`
	Sequence            uint64         `json:"sequence,string"`
	StartedAt           time.Time      `json:"startedAt"`
	CompletedAt         time.Time      `json:"completedAt"`
	LogicalProcessors   uint32         `json:"logicalProcessors"`
	EnumerationComplete bool           `json:"enumerationComplete"`
	TotalEnumerated     int            `json:"totalEnumerated"`
	LimitReason         string         `json:"limitReason,omitempty"`
	Rows                []ProcessUsage `json:"rows"`
}

type ProcessListQuery struct {
	SnapshotID string  `json:"snapshotId"`
	Cursor     string  `json:"cursor"`
	Limit      int     `json:"limit"`
	Name       string  `json:"name"`
	PID        *uint32 `json:"pid"`
	SortBy     string  `json:"sortBy"`
	Descending bool    `json:"descending"`
}

type ProcessPage struct {
	SchemaVersion       int            `json:"schemaVersion"`
	CollectorEpoch      string         `json:"collectorEpoch"`
	SnapshotID          string         `json:"snapshotId"`
	Sequence            uint64         `json:"sequence,string"`
	StartedAt           time.Time      `json:"startedAt"`
	CompletedAt         time.Time      `json:"completedAt"`
	AgeMs               float64        `json:"ageMs"`
	EnumerationComplete bool           `json:"enumerationComplete"`
	TotalEnumerated     int            `json:"totalEnumerated"`
	TotalMatched        int            `json:"totalMatched"`
	LimitReason         string         `json:"limitReason,omitempty"`
	Rows                []ProcessUsage `json:"rows"`
	NextCursor          string         `json:"nextCursor,omitempty"`
}

type Capability struct {
	Supported bool   `json:"supported"`
	Source    string `json:"source"`
	Reason    string `json:"reason,omitempty"`
}

type Capabilities struct {
	SchemaVersion     int        `json:"schemaVersion"`
	ABIVersion        int        `json:"abiVersion"`
	Platform          string     `json:"platform"`
	CPU               Capability `json:"cpu"`
	Memory            Capability `json:"memory"`
	GPU               Capability `json:"gpu"`
	GenericIO         Capability `json:"genericIo"`
	Disk              Capability `json:"disk"`
	Network           Capability `json:"network"`
	TreeGroups        bool       `json:"treeGroups"`
	SessionGroups     bool       `json:"sessionGroups"`
	MinimumIntervalMs int        `json:"minimumIntervalMs"`
	MaximumProcesses  int        `json:"maximumProcesses"`
}

type ProcessSelector struct {
	PID                uint32 `json:"pid"`
	ExpectedProcessKey string `json:"expectedProcessKey"`
	SnapshotID         string `json:"snapshotId"`
}

type ProcessGroupQuery struct {
	RootProcessKey string `json:"rootProcessKey"`
	SnapshotID     string `json:"snapshotId"`
}

type ProcessGroup struct {
	Disk                   ProcessIO      `json:"disk"`
	Network                ProcessNetwork `json:"network"`
	SnapshotID             string         `json:"snapshotId"`
	RootProcessKey         string         `json:"rootProcessKey"`
	Kind                   string         `json:"kind"`
	MemberCount            int            `json:"memberCount"`
	MembershipComplete     bool           `json:"membershipComplete"`
	Members                []string       `json:"members"`
	CPUPercent             *float64       `json:"cpuPercent"`
	PrivateWorkingSetBytes *uint64        `json:"privateWorkingSetBytes,string"`
	CPUStatus              Status         `json:"cpuStatus"`
	MemoryStatus           Status         `json:"memoryStatus"`
	GPU                    ProcessGPU     `json:"gpu"`
}
