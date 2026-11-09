//go:build windows && amd64 && !cgo

// SPDX-License-Identifier: BSD-3-Clause
package resourcecollector

import "time"

type etwSource struct{}

func newETWSource() *etwSource { return &etwSource{} }
func (e *etwSource) capability(source string) Capability {
	return Capability{Source: source, Reason: "CGO_REQUIRED"}
}
func (e *etwSource) capture(rows []RawProcess) {
	for i := range rows {
		m := unavailable(Unsupported, "windows.etw", "CGO_REQUIRED", time.Now())
		rows[i].Disk = ProcessIO{Metric: m}
		rows[i].Network = ProcessNetwork{Metric: m}
	}
}
func (e *etwSource) close()      {}
func (e *etwSource) clockCheck() {}
