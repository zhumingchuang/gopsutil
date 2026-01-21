package main

/*
#include <stdlib.h>
#include <string.h>
*/
import "C"
import (
	"encoding/json"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/gpu"
	"github.com/shirou/gopsutil/v4/mem"
)

type CPUUsage struct {
	Percent []float64 `json:"percent"`
	Message string    `json:"message"`
}

type Resp struct {
	Infos   any    `json:"infos,omitempty"`
	Total   any    `json:"total,omitempty"`
	Message string `json:"message,omitempty"`
}

type MemoryUsage struct {
	UsedMB  uint64 `json:"used_mb"`
	TotalMB uint64 `json:"total_mb"`
	Message string `json:"message"`
}

// 内部辅助函数：将 Go 字符串安全拷贝到 C Buffer 中
func copyToCBuffer(buf *C.char, maxLen C.int, content []byte) C.int {
	if buf == nil || maxLen <= 0 {
		return 0
	}
	capacity := int(maxLen)

	// capacity==1 时只能放 '\0'
	limit := capacity - 1
	if limit <= 0 {
		*(*byte)(unsafe.Pointer(buf)) = 0
		return 0
	}

	n := len(content)
	if n > limit {
		n = limit
	}

	dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), capacity)
	copy(dst[:n], content[:n])
	dst[n] = 0
	return C.int(n)
}

func detectTools() (hasNvidia bool, hasAmd bool) {
	hasNvidia = gpu.HasCommand("nvidia-smi")
	hasAmd = gpu.HasCommand("amd-smi") || gpu.HasCommand("rocm-smi")
	return
}

func writeJSON(buf *C.char, maxLen C.int, v any) C.int {
	b, _ := json.Marshal(v)
	return copyToCBuffer(buf, maxLen, b)
}

//export GetCPUUsage
func GetCPUUsage(buf *C.char, maxLen C.int) C.int {
	var res CPUUsage
	percent, err := cpu.Percent(time.Second, false)
	if err != nil {
		res.Message = err.Error()
	} else {
		res.Percent = percent
	}
	jsonData, _ := json.Marshal(res)
	return copyToCBuffer(buf, maxLen, jsonData)
}

//export GetGPUStaticInfo
func GetGPUStaticInfo(buf *C.char, maxLen C.int) C.int {
	var res Resp
	hasNvidia, hasAmd := detectTools()

	if !hasNvidia && !hasAmd {
		res.Message = "no gpu tools found"
		return writeJSON(buf, maxLen, res)
	}

	infos, err := gpu.QueryStaticInfo(hasNvidia, hasAmd)
	if err != nil {
		res.Message = err.Error()
		return writeJSON(buf, maxLen, res)
	}

	res.Infos = infos
	return writeJSON(buf, maxLen, res)
}

//export GetGPUDynamicAll
func GetGPUDynamicAll(buf *C.char, maxLen C.int) C.int {
	var res Resp
	hasNvidia, hasAmd := detectTools()

	if !hasNvidia && !hasAmd {
		res.Message = "no gpu tools found"
		return writeJSON(buf, maxLen, res)
	}

	infos, err := gpu.QueryDynamicAll(hasNvidia, hasAmd)
	if err != nil {
		res.Message = err.Error()
		return writeJSON(buf, maxLen, res)
	}

	res.Infos = infos
	return writeJSON(buf, maxLen, res)
}

//export GetGPUDynamicTotalAvg
func GetGPUDynamicTotalAvg(buf *C.char, maxLen C.int) C.int {
	var res Resp
	hasNvidia, hasAmd := detectTools()

	if !hasNvidia && !hasAmd {
		res.Message = "no gpu tools found"
		return writeJSON(buf, maxLen, res)
	}

	total, err := gpu.QueryDynamicTotalAvg(hasNvidia, hasAmd)
	if err != nil {
		res.Message = err.Error()
		return writeJSON(buf, maxLen, res)
	}

	res.Total = total
	return writeJSON(buf, maxLen, res)
}

//export GetMemUsage
func GetMemUsage(buf *C.char, maxLen C.int) C.int {
	var res MemoryUsage
	v, err := mem.VirtualMemory()
	if err != nil {
		res.Message = err.Error()
	} else {
		res.TotalMB = v.Total / 1024 / 1024
		res.UsedMB = v.Used / 1024 / 1024
	}
	jsonData, _ := json.Marshal(res)
	return copyToCBuffer(buf, maxLen, jsonData)
}

func main() {
}
