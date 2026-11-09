// SPDX-License-Identifier: BSD-3-Clause
//go:build windows && amd64

package resourcecollector

import (
	"fmt"
	"math"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var pdhDLL = windows.NewLazySystemDLL("pdh.dll")
var pdhOpen = pdhDLL.NewProc("PdhOpenQueryW")
var pdhAdd = pdhDLL.NewProc("PdhAddEnglishCounterW")
var pdhCollect = pdhDLL.NewProc("PdhCollectQueryData")
var pdhArray = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
var pdhClose = pdhDLL.NewProc("PdhCloseQuery")

const pdhMoreData = 0x800007d2
const pdhNoData = 0x800007d5

// PDH_FMT_COUNTERVALUE_ITEM_W on Windows x64. The union is represented as bits
// so integer byte counters never pass through a floating point conversion.
type pdhItem struct {
	Name    *uint16
	Status  uint32
	Padding uint32
	Value   uint64
}
type counterValues struct {
	Values map[string]uint64
	Err    error
}
type pdhQuery struct {
	handle   uintptr
	counters map[string]uintptr
	errors   map[string]error
}

func newPDHQuery(paths map[string]string) (*pdhQuery, error) {
	q := &pdhQuery{counters: make(map[string]uintptr), errors: make(map[string]error)}
	status, _, _ := pdhOpen.Call(0, 0, uintptr(unsafe.Pointer(&q.handle)))
	if status != 0 {
		return nil, fmt.Errorf("PDH_OPEN_%08X", status)
	}
	for key, path := range paths {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			q.errors[key] = err
			continue
		}
		var handle uintptr
		status, _, _ := pdhAdd.Call(q.handle, uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&handle)))
		if status != 0 {
			q.errors[key] = fmt.Errorf("PDH_ADD_%08X", status)
		} else {
			q.counters[key] = handle
		}
	}
	return q, nil
}

func (q *pdhQuery) collect() error {
	status, _, _ := pdhCollect.Call(q.handle)
	if status != 0 {
		return fmt.Errorf("PDH_COLLECT_%08X", status)
	}
	return nil
}

func (q *pdhQuery) read(key string, integer bool) counterValues {
	out := counterValues{Values: make(map[string]uint64)}
	handle, ok := q.counters[key]
	if !ok {
		out.Err = q.errors[key]
		if out.Err == nil {
			out.Err = fmt.Errorf("PDH_COUNTER_UNAVAILABLE")
		}
		return out
	}
	format := uintptr(0x200 | 0x8000) // PDH_FMT_DOUBLE | PDH_FMT_NOCAP100
	if integer {
		format = 0x400 | 0x8000
	} // PDH_FMT_LARGE; preserve byte precision
	// Counter instances can appear between the sizing and reading calls. Retry
	// from a fresh zero-size request, rather than trusting the failed buffer size.
	for attempt := 0; attempt < 3; attempt++ {
		var size, count uint32
		status, _, _ := pdhArray.Call(handle, format, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
		if status == pdhNoData {
			out.Err = fmt.Errorf("PDH_NO_DATA")
			return out
		}
		if status != pdhMoreData {
			out.Err = fmt.Errorf("PDH_SIZE_%08X", status)
			return out
		}
		if size == 0 || size > 8*1024*1024 {
			out.Err = fmt.Errorf("PDH_BUFFER_LIMIT")
			return out
		}
		buffer := make([]byte, size)
		status, _, _ = pdhArray.Call(handle, format, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buffer[0])))
		if status == pdhMoreData {
			continue
		}
		if status != 0 {
			out.Err = fmt.Errorf("PDH_READ_%08X", status)
			return out
		}
		if uint64(count)*uint64(unsafe.Sizeof(pdhItem{})) > uint64(len(buffer)) {
			out.Err = fmt.Errorf("PDH_ARRAY_INVALID")
			return out
		}
		items := unsafe.Slice((*pdhItem)(unsafe.Pointer(&buffer[0])), int(count))
		duplicates := make(map[string]bool)
		for _, item := range items {
			if item.Status != 0 && item.Status != 1 {
				out.Err = fmt.Errorf("PDH_INSTANCE_%08X", item.Status)
				continue
			}
			if integer && item.Value > math.MaxInt64 {
				out.Err = fmt.Errorf("PDH_NEGATIVE_VALUE")
				continue
			}
			name, err := pdhName(buffer, item.Name)
			if err != nil {
				out.Err = err
				continue
			}
			if _, exists := out.Values[name]; exists {
				duplicates[name] = true
				delete(out.Values, name)
				out.Err = fmt.Errorf("PDH_DUPLICATE_INSTANCE")
				continue
			}
			if !duplicates[name] {
				out.Values[name] = item.Value
			}
		}
		runtime.KeepAlive(buffer)
		return out
	}
	out.Err = fmt.Errorf("PDH_INSTANCE_CHURN")
	return out
}

func pdhName(buffer []byte, p *uint16) (string, error) {
	start := uintptr(unsafe.Pointer(&buffer[0]))
	address := uintptr(unsafe.Pointer(p))
	end := start + uintptr(len(buffer))
	if address < start || address >= end || (address-start)%2 != 0 {
		return "", fmt.Errorf("PDH_NAME_INVALID")
	}
	chars := unsafe.Slice(p, int((end-address)/2))
	for i, v := range chars {
		if v == 0 {
			return windows.UTF16ToString(chars[:i]), nil
		}
	}
	return "", fmt.Errorf("PDH_NAME_UNTERMINATED")
}

func (q *pdhQuery) close() error {
	if q == nil || q.handle == 0 {
		return nil
	}
	status, _, _ := pdhClose.Call(q.handle)
	q.handle = 0
	if status != 0 {
		return fmt.Errorf("PDH_CLOSE_%08X", status)
	}
	return nil
}
func pdhDouble(bits uint64) float64 { return math.Float64frombits(bits) }
