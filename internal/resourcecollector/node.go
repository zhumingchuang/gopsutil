// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import (
	"encoding/json"
	"time"
)

type Gauge struct {
	Metric
	Percent *float64 `json:"percent"`
}
type NodeMemory struct {
	Metric
	TotalBytes       *uint64  `json:"totalBytes,string"`
	AvailableBytes   *uint64  `json:"availableBytes,string"`
	UsedPercent      *float64 `json:"usedPercent"`
	CommitTotalBytes *uint64  `json:"commitTotalBytes,string"`
	CommitLimitBytes *uint64  `json:"commitLimitBytes,string"`
}
type Volume struct {
	Metric
	DiskMapping          Metric   `json:"diskMapping"`
	DiskNumbers          []uint32 `json:"diskNumbers"`
	ID                   string   `json:"id"`
	MountPaths           []string `json:"mountPaths"`
	TotalBytes           *uint64  `json:"totalBytes,string"`
	FreeBytes            *uint64  `json:"freeBytes,string"`
	CallerAvailableBytes *uint64  `json:"callerAvailableBytes,string"`
	UsedPercent          *float64 `json:"usedPercent"`
}
type PhysicalDisk struct {
	Metric
	Instance            string   `json:"instance"`
	BusyPercent         *float64 `json:"busyPercent"`
	ReadBytesPerSecond  *float64 `json:"readBytesPerSecond"`
	WriteBytesPerSecond *float64 `json:"writeBytesPerSecond"`
}
type NetworkInterface struct {
	Metric
	ID                  string   `json:"id"`
	LUID                uint64   `json:"luid,string"`
	Index               uint32   `json:"index"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Type                uint32   `json:"type"`
	Hardware            bool     `json:"hardware"`
	OperStatus          uint32   `json:"operStatus"`
	RxLinkBitsPerSecond uint64   `json:"rxLinkBitsPerSecond,string"`
	TxLinkBitsPerSecond uint64   `json:"txLinkBitsPerSecond,string"`
	RxBytesTotal        uint64   `json:"rxBytesTotal,string"`
	TxBytesTotal        uint64   `json:"txBytesTotal,string"`
	RxBytesPerSecond    *float64 `json:"rxBytesPerSecond"`
	TxBytesPerSecond    *float64 `json:"txBytesPerSecond"`
	RxPercent           *float64 `json:"rxPercent"`
	TxPercent           *float64 `json:"txPercent"`
}
type NodeGPU struct {
	Metric
	ID             string   `json:"id"`
	Percent        *float64 `json:"percent"`
	DedicatedBytes *uint64  `json:"dedicatedBytes,string"`
	SharedBytes    *uint64  `json:"sharedBytes,string"`
}
type NodeSnapshot struct {
	SchemaVersion     int                `json:"schemaVersion"`
	CollectorEpoch    string             `json:"collectorEpoch"`
	Sequence          uint64             `json:"sequence,string"`
	CollectedAt       time.Time          `json:"collectedAt"`
	LogicalProcessors uint32             `json:"logicalProcessors"`
	CPU               Gauge              `json:"cpu"`
	Memory            NodeMemory         `json:"memory"`
	VolumesMetric     Metric             `json:"volumesMetric"`
	Volumes           []Volume           `json:"volumes"`
	DisksMetric       Metric             `json:"disksMetric"`
	Disks             []PhysicalDisk     `json:"disks"`
	NetworkMetric     Metric             `json:"networkMetric"`
	Network           []NetworkInterface `json:"network"`
	GPUMetric         Metric             `json:"gpuMetric"`
	GPU               []NodeGPU          `json:"gpu"`
}

// GetNodeUsage shares the frozen capture generation with the process API.
func (c *Collector) GetNodeUsage() (NodeSnapshot, error) {
	s, err := c.readSnapshot("", false)
	if err != nil {
		return NodeSnapshot{}, err
	}
	body, err := json.Marshal(s.Node)
	if err != nil {
		return NodeSnapshot{}, err
	}
	var copy NodeSnapshot
	err = json.Unmarshal(body, &copy)
	return copy, err
}
