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

type GPUUsage struct {
	Infos   []gpu.GPUInfo `json:"infos"`
	Message string        `json:"message"`
}

type MemoryUsage struct {
	UsedMB  uint64 `json:"used_mb"`
	TotalMB uint64 `json:"total_mb"`
	Message string `json:"message"`
}

// 内部辅助函数：将 Go 字符串安全拷贝到 C Buffer 中
func copyToCBuffer(buf *C.char, maxLen C.int, content []byte) C.int {
	actualLen := len(content)
	limit := int(maxLen) - 1 // 预留一个位置给 \0

	copySize := actualLen
	if copySize > limit {
		copySize = limit
	}

	// 获取 C 指针对应的 Go slice
	outSlice := (*[1 << 30]byte)(unsafe.Pointer(buf))[:copySize:copySize]
	copy(outSlice, content[:copySize])

	// 强制添加 null 终止符（C 字符串标准）
	(*[1 << 30]byte)(unsafe.Pointer(buf))[copySize] = 0

	return C.int(copySize)
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

//export GetGPUUsage
func GetGPUUsage(buf *C.char, maxLen C.int) C.int {
	var res GPUUsage
	hasNvidia := gpu.HasCommand("nvidia-smi")
	hasAmd := gpu.HasCommand("rocm-smi")

	if !hasNvidia && !hasAmd {
		res.Message = "no gpu tools found"
	} else {
		infos, err := gpu.QueryOnce(hasNvidia, hasAmd)
		if err != nil {
			res.Message = err.Error()
		} else {
			res.Infos = infos
		}
	}
	jsonData, _ := json.Marshal(res)
	return copyToCBuffer(buf, maxLen, jsonData)
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

func main() {}
