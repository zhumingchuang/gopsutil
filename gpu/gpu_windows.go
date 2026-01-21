// SPDX-License-Identifier: BSD-3-Clause
//go:build windows

package gpu

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// GPUStaticInfo 不会频繁变化的显卡信息
type GPUStaticInfo struct {
	Vendor     string `json:"vendor"`          // 厂商标识："nvidia" 或 "amd"
	Index      int    `json:"index"`           // 显卡索引（第几张显卡）
	Name       string `json:"name"`            // 显卡名称/型号
	MemTotalMB int    `json:"memory_total_mb"` // 显存总量（MB）
}

// GPUDynamicInfo 实时变化的显卡指标
type GPUDynamicInfo struct {
	Vendor      string  `json:"vendor"`              // 厂商标识："nvidia" 或 "amd"
	Index       int     `json:"index"`               // 显卡索引（第几张显卡）
	Utilization int     `json:"utilization_percent"` // GPU 使用率（百分比）
	MemUsedMB   int     `json:"memory_used_mb"`      // 显存已用（MB）
	TempC       int     `json:"temperature_c"`       // 当前温度（摄氏度）
	PowerW      float64 `json:"power_w"`             // 当前功耗（瓦特）
}

// GPUDynamicTotal “所有显卡”的动态指标聚合结果（平均值）
type GPUDynamicTotal struct {
	UtilizationAvg    int `json:"utilization_avg_percent"`        // GPU 使用率平均值（百分比），无法获取为 -1
	MemUtilizationAvg int `json:"memory_utilization_avg_percent"` // 显存使用率平均值（百分比），无法获取为 -1
}

// unknownInt/unknownFloat 用于表示采集失败或缺失值的哨兵值。
const unknownInt = -1
const unknownFloat = -1

type gpuAllFields struct {
	Index       int
	Name        string
	Utilization int
	MemTotalMB  int
	MemUsedMB   int
	TempC       int
	PowerW      float64
}

var (
	staticMu    sync.Mutex
	staticCache = map[staticCacheKey]staticCacheEntry{}
)

type staticCacheKey struct {
	hasNvidia bool
	hasAmd    bool
}

type staticCacheEntry struct {
	infos []GPUStaticInfo
	err   error
}

type gpuCardKey struct {
	Vendor string
	Index  int
}

// HasCommand 检查系统 PATH 中是否存在指定命令。
func HasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// QueryStaticInfo 获取 GPU 静态信息（名称、总显存等），结果会在进程内缓存一次。
// hasNvidia: 是否使用 nvidia-smi 作为数据源（通常由 HasCommand("nvidia-smi") 传入）。
// hasAmd: 是否使用 rocm-smi 作为数据源（通常由 HasCommand("rocm-smi") 传入）。
func QueryStaticInfo(hasNvidia, hasAmd bool) ([]GPUStaticInfo, error) {
	resolvedNvidia := hasNvidia || HasCommand("nvidia-smi")
	resolvedAmd := hasAmd || HasCommand("amd-smi") || HasCommand("rocm-smi")
	key := staticCacheKey{hasNvidia: resolvedNvidia, hasAmd: resolvedAmd}

	staticMu.Lock()
	if cached, ok := staticCache[key]; ok {
		staticMu.Unlock()
		if cached.err != nil {
			return nil, cached.err
		}
		out := make([]GPUStaticInfo, len(cached.infos))
		copy(out, cached.infos)
		return out, nil
	}
	staticMu.Unlock()

	infos, err := queryStaticInfoOnce(resolvedNvidia, resolvedAmd)

	staticMu.Lock()
	staticCache[key] = staticCacheEntry{infos: infos, err: err}
	staticMu.Unlock()

	if err != nil {
		return nil, err
	}
	out := make([]GPUStaticInfo, len(infos))
	copy(out, infos)
	return out, nil
}

// QueryDynamicAll 获取所有显卡的动态指标（逐卡返回数组）。
// hasNvidia: 是否使用 nvidia-smi 作为数据源。
// hasAmd: 是否使用 rocm-smi 作为数据源。
func QueryDynamicAll(hasNvidia, hasAmd bool) ([]GPUDynamicInfo, error) {
	tryNvidia := hasNvidia || HasCommand("nvidia-smi")
	tryAmd := hasAmd || HasCommand("amd-smi") || HasCommand("rocm-smi")

	if tryNvidia && tryAmd {
		var out []GPUDynamicInfo
		var nErr error
		var aErr error

		nInfos, err := queryNvidiaDynamic()
		if err != nil {
			nErr = err
		} else {
			out = append(out, nInfos...)
		}

		aRaw, err := queryAmdRaw()
		if err != nil {
			aErr = err
		} else {
			for _, g := range aRaw {
				out = append(out, GPUDynamicInfo{
					Vendor:      "amd",
					Index:       g.Index,
					Utilization: g.Utilization,
					MemUsedMB:   g.MemUsedMB,
					TempC:       g.TempC,
					PowerW:      g.PowerW,
				})
			}
		}

		if len(out) > 0 {
			return out, nil
		}
		if nErr != nil && aErr != nil {
			return nil, fmt.Errorf("no supported GPU query tool available: nvidia error: %v; amd error: %v", nErr, aErr)
		}
		if nErr != nil {
			return nil, nErr
		}
		return nil, aErr
	}

	if tryNvidia {
		return queryNvidiaDynamic()
	}
	if tryAmd {
		all, err := queryAmdRaw()
		if err != nil {
			return nil, err
		}
		out := make([]GPUDynamicInfo, 0, len(all))
		for _, g := range all {
			out = append(out, GPUDynamicInfo{
				Vendor:      "amd",
				Index:       g.Index,
				Utilization: g.Utilization,
				MemUsedMB:   g.MemUsedMB,
				TempC:       g.TempC,
				PowerW:      g.PowerW,
			})
		}
		return out, nil
	}
	return nil, fmt.Errorf("no supported GPU query tool available")
}

// QueryDynamicTotalAvg 获取所有显卡的动态指标聚合结果（平均值）。
// hasNvidia: 是否使用 nvidia-smi 作为数据源。
// hasAmd: 是否使用 rocm-smi 作为数据源。
func QueryDynamicTotalAvg(hasNvidia, hasAmd bool) (GPUDynamicTotal, error) {
	infos, err := QueryDynamicAll(hasNvidia, hasAmd)
	if err != nil {
		return GPUDynamicTotal{}, err
	}
	staticInfos, err := QueryStaticInfo(hasNvidia, hasAmd)
	if err != nil {
		return GPUDynamicTotal{}, err
	}
	memTotalByCard := make(map[gpuCardKey]int, len(staticInfos))
	for _, s := range staticInfos {
		memTotalByCard[gpuCardKey{Vendor: s.Vendor, Index: s.Index}] = s.MemTotalMB
	}
	return aggregateDynamic(infos, memTotalByCard), nil
}

// queryStaticInfoOnce 获取静态信息的实际实现（不缓存）。
func queryStaticInfoOnce(hasNvidia, hasAmd bool) ([]GPUStaticInfo, error) {
	if hasNvidia && hasAmd {
		var out []GPUStaticInfo
		var nErr error
		var aErr error

		nInfos, err := queryNvidiaStatic()
		if err != nil {
			nErr = err
		} else {
			out = append(out, nInfos...)
		}

		aRaw, err := queryAmdRaw()
		if err != nil {
			aErr = err
		} else {
			for _, g := range aRaw {
				out = append(out, GPUStaticInfo{
					Vendor:     "amd",
					Index:      g.Index,
					Name:       g.Name,
					MemTotalMB: g.MemTotalMB,
				})
			}
		}

		if len(out) > 0 {
			return out, nil
		}
		if nErr != nil && aErr != nil {
			return nil, fmt.Errorf("no supported GPU query tool available: nvidia error: %v; amd error: %v", nErr, aErr)
		}
		if nErr != nil {
			return nil, nErr
		}
		return nil, aErr
	}

	if hasNvidia {
		return queryNvidiaStatic()
	}
	if hasAmd {
		infos, err := queryAmdRaw()
		if err != nil {
			return nil, err
		}
		out := make([]GPUStaticInfo, 0, len(infos))
		for _, g := range infos {
			out = append(out, GPUStaticInfo{
				Vendor:     "amd",
				Index:      g.Index,
				Name:       g.Name,
				MemTotalMB: g.MemTotalMB,
			})
		}
		return out, nil
	}
	return nil, fmt.Errorf("no supported GPU query tool available")
}

/* -------- NVIDIA -------- */

// queryNvidiaStatic 只获取 NVIDIA 静态字段（index/name/memory.total）。
func queryNvidiaStatic() ([]GPUStaticInfo, error) {
	args := []string{
		"--query-gpu=index,name,memory.total",
		"--format=csv,noheader,nounits",
	}
	cmd := exec.Command("nvidia-smi", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("nvidia-smi failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var infos []GPUStaticInfo
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitCSVLine(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("unexpected nvidia-smi output line: %q", line)
		}

		info := GPUStaticInfo{
			Vendor:     "nvidia",
			MemTotalMB: unknownInt,
		}
		if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
			info.Index = v
		}
		info.Name = strings.TrimSpace(fields[1])
		if v, err := parseIntRelax(fields[2]); err == nil {
			info.MemTotalMB = v
		}

		infos = append(infos, info)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return infos, nil
}

// queryNvidiaDynamic 只获取 NVIDIA 动态字段（index/utilization/memory.used/temperature/power）。
func queryNvidiaDynamic() ([]GPUDynamicInfo, error) {
	args := []string{
		"--query-gpu=index,utilization.gpu,memory.used,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits",
	}
	cmd := exec.Command("nvidia-smi", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("nvidia-smi failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var infos []GPUDynamicInfo
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitCSVLine(line)
		if len(fields) < 5 {
			return nil, fmt.Errorf("unexpected nvidia-smi output line: %q", line)
		}

		info := GPUDynamicInfo{
			Vendor:      "nvidia",
			Utilization: unknownInt,
			MemUsedMB:   unknownInt,
			TempC:       unknownInt,
			PowerW:      unknownFloat,
		}

		if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
			info.Index = v
		}
		if v, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil {
			info.Utilization = v
		}
		if v, err := parseIntRelax(fields[2]); err == nil {
			info.MemUsedMB = v
		}
		if v, err := strconv.Atoi(strings.TrimSpace(fields[3])); err == nil {
			info.TempC = v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64); err == nil {
			info.PowerW = v
		}

		infos = append(infos, info)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return infos, nil
}

// splitCSVLine 简单按逗号拆分一行 CSV，并对每个字段做 TrimSpace。
func splitCSVLine(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// parseIntRelax 将字符串解析为 int，兼容空值和 "N/A"，也兼容浮点字符串（四舍五入）。
func parseIntRelax(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("no value")
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f + 0.5), nil
	}
	return 0, fmt.Errorf("cannot parse int from %q", s)
}

/* -------- AMD (amd-smi / rocm-smi, heuristic parsing) -------- */

// queryAmdRaw 调用 rocm-smi 并解析其文本输出为显卡信息（静态+动态字段），供静态/动态接口复用。
func queryAmdRaw() ([]gpuAllFields, error) {
	var lastErr error

	try := func(name string, args ...string) ([]gpuAllFields, error) {
		cmd := exec.Command(name, args...)
		out, err := cmd.CombinedOutput()
		if err != nil && len(out) == 0 {
			if ee, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("%s failed: %s", name, strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, err
		}
		text := string(out)
		infos := parseRocmSmiTable(text)
		if len(infos) == 0 {
			snippet := text
			if len(snippet) > 800 {
				snippet = snippet[:800] + "..."
			}
			return nil, fmt.Errorf("failed to parse %s output (output snippet: %q)", name, snippet)
		}
		return infos, nil
	}

	if HasCommand("amd-smi") {
		infos, err := try("amd-smi", "--showproductname", "--showuse", "--showmemuse", "--showtemp", "--showpower", "--showmeminfo", "vram")
		if err == nil {
			return infos, nil
		}
		infos, err = try("amd-smi")
		if err == nil {
			return infos, nil
		}
		lastErr = err
	}
	if HasCommand("rocm-smi") {
		infos, err := try("rocm-smi", "--showproductname", "--showuse", "--showmemuse", "--showtemp", "--showpower", "--showmeminfo", "vram")
		if err == nil {
			return infos, nil
		}
		infos, err = try("rocm-smi")
		if err == nil {
			return infos, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no supported GPU query tool available")
}

// parseRocmSmiTable 将 rocm-smi 的表格/文本输出解析为显卡信息列表。
func parseRocmSmiTable(text string) []gpuAllFields {
	scanner := bufio.NewScanner(strings.NewReader(text))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	nameMap := map[int]string{}
	reName1 := regexp.MustCompile(`(?i)^\s*GPU\s*(\d+)\s*[:\-]\s*(.+)$`)
	reName2 := regexp.MustCompile(`(?i)^\s*Card\s*(\d+)\s*[:\-]\s*(.+)$`)
	reName3 := regexp.MustCompile(`(?i)^\s*(\d+)\)\s*(.+)$`)
	reInline := regexp.MustCompile(`(?i)GPU\s*(\d+)[:\s,]+(.+)`)
	for _, ln := range lines {
		if m := reName1.FindStringSubmatch(ln); len(m) == 3 {
			if idx, err := strconv.Atoi(strings.TrimSpace(m[1])); err == nil {
				nameMap[idx] = strings.TrimSpace(m[2])
			}
		} else if m := reName2.FindStringSubmatch(ln); len(m) == 3 {
			if idx, err := strconv.Atoi(strings.TrimSpace(m[1])); err == nil {
				nameMap[idx] = strings.TrimSpace(m[2])
			}
		} else if m := reName3.FindStringSubmatch(ln); len(m) == 3 {
			if idx, err := strconv.Atoi(strings.TrimSpace(m[1])); err == nil {
				nameMap[idx] = strings.TrimSpace(m[2])
			}
		}
	}

	reIndexLine := regexp.MustCompile(`^\s*(\d+)\s+(.+)$`)

	var infos []gpuAllFields
	for _, ln := range lines {
		lnTrim := strings.TrimSpace(ln)
		if lnTrim == "" {
			continue
		}
		if m := reIndexLine.FindStringSubmatch(ln); len(m) == 3 {
			idx, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			rest := m[2]
			info := gpuAllFields{
				Index:       idx,
				Utilization: unknownInt,
				MemTotalMB:  unknownInt,
				MemUsedMB:   unknownInt,
				TempC:       unknownInt,
				PowerW:      unknownFloat,
			}
			if n, ok := nameMap[idx]; ok {
				info.Name = n
			}
			info.Utilization = findFirstPercent(rest)
			used, total := findMemoryUsedAndTotal(rest)
			info.MemUsedMB = used
			info.MemTotalMB = total
			info.TempC = findTemp(rest)
			info.PowerW = findPower(rest)
			infos = appendOrMerge(infos, info)
		}
		if m := reInline.FindStringSubmatch(ln); len(m) == 3 {
			idx, err := strconv.Atoi(strings.TrimSpace(m[1]))
			if err != nil {
				continue
			}
			rest := m[2]
			info := gpuAllFields{
				Index:       idx,
				Utilization: unknownInt,
				MemTotalMB:  unknownInt,
				MemUsedMB:   unknownInt,
				TempC:       unknownInt,
				PowerW:      unknownFloat,
			}
			if n, ok := nameMap[idx]; ok {
				info.Name = n
			}
			info.Utilization = findFirstPercent(rest)
			used, total := findMemoryUsedAndTotal(rest)
			info.MemUsedMB = used
			info.MemTotalMB = total
			info.TempC = findTemp(rest)
			info.PowerW = findPower(rest)
			infos = appendOrMerge(infos, info)
		}
	}

	byIndex := map[int]gpuAllFields{}
	for _, g := range infos {
		if ex, ok := byIndex[g.Index]; ok {
			byIndex[g.Index] = mergeGPUInfo(ex, g)
		} else {
			byIndex[g.Index] = g
		}
	}
	var ordered []gpuAllFields
	for _, g := range byIndex {
		ordered = append(ordered, g)
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].Index < ordered[i].Index {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	return ordered
}

func appendOrMerge(list []gpuAllFields, g gpuAllFields) []gpuAllFields {
	for i := range list {
		if list[i].Index == g.Index {
			list[i] = mergeGPUInfo(list[i], g)
			return list
		}
	}
	return append(list, g)
}

func mergeGPUInfo(a, b gpuAllFields) gpuAllFields {
	if b.Name != "" {
		a.Name = b.Name
	}
	if b.Utilization != unknownInt {
		a.Utilization = b.Utilization
	}
	if b.MemTotalMB != unknownInt {
		a.MemTotalMB = b.MemTotalMB
	}
	if b.MemUsedMB != unknownInt {
		a.MemUsedMB = b.MemUsedMB
	}
	if b.TempC != unknownInt {
		a.TempC = b.TempC
	}
	if b.PowerW != unknownFloat {
		a.PowerW = b.PowerW
	}
	return a
}

var rePercent = regexp.MustCompile(`(\d{1,3})\s*%`)
var reMemSlash = regexp.MustCompile(`(?i)(\d+)\s*(?:MiB|MB)?\s*[/]\s*(\d+)\s*(?:MiB|MB)?`)
var reMemNamed = regexp.MustCompile(`(?i)VRAM[:\s]*\s*(\d+)\s*(?:MiB|MB)?\s*[/]\s*(\d+)\s*(?:MiB|MB)?`)
var reMemUsed = regexp.MustCompile(`(?i)\b(?:vram|mem(?:ory)?)\b[^0-9]*(?:used|use|usage)\b[^0-9]*(\d+(?:\.\d+)?)\s*(MiB|MB|GiB|GB)\b`)
var reMemTotal = regexp.MustCompile(`(?i)\b(?:vram|mem(?:ory)?)\b[^0-9]*(?:total|size)\b[^0-9]*(\d+(?:\.\d+)?)\s*(MiB|MB|GiB|GB)\b`)
var reTempC = regexp.MustCompile(`(?i)(\d{1,3}(?:\.\d+)?)\s*°?C|\b(\d{1,3}(?:\.\d+)?)\s*[Cc]\b`)
var rePowerW = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*W\b`)

func findFirstPercent(s string) int {
	if m := rePercent.FindStringSubmatch(s); len(m) >= 2 {
		if v, err := strconv.Atoi(m[1]); err == nil {
			return v
		}
	}
	return unknownInt
}

// findMemoryUsedAndTotal 从文本中提取 "used/total" 形式的显存用量（单位按 MB/MiB 直接当作数值）。
func findMemoryUsedAndTotal(s string) (used, total int) {
	used, total = unknownInt, unknownInt
	if m := reMemNamed.FindStringSubmatch(s); len(m) == 3 {
		if u, err := strconv.Atoi(m[1]); err == nil {
			used = u
		}
		if t, err := strconv.Atoi(m[2]); err == nil {
			total = t
		}
		return
	}
	if m := reMemSlash.FindStringSubmatch(s); len(m) == 3 {
		if u, err := strconv.Atoi(m[1]); err == nil {
			used = u
		}
		if t, err := strconv.Atoi(m[2]); err == nil {
			total = t
		}
		return
	}
	if u := findMemMB(reMemUsed, s); u != unknownInt {
		used = u
	}
	if t := findMemMB(reMemTotal, s); t != unknownInt {
		total = t
	}
	return
}

func findMemMB(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if len(m) != 3 {
		return unknownInt
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return unknownInt
	}
	unit := strings.ToLower(strings.TrimSpace(m[2]))
	switch unit {
	case "gib", "gb":
		return int(f*1024 + 0.5)
	default:
		return int(f + 0.5)
	}
}

// findTemp 从文本中提取温度（摄氏度），支持整数/小数，四舍五入。
func findTemp(s string) int {
	if m := reTempC.FindStringSubmatch(s); len(m) >= 2 {
		// m[1] may be empty if second group matched
		val := ""
		if m[1] != "" {
			val = m[1]
		} else {
			val = m[2]
		}
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return int(f + 0.5)
		}
	}
	return unknownInt
}

// findPower 从文本中提取功耗（瓦特），支持小数。
func findPower(s string) float64 {
	if m := rePowerW.FindStringSubmatch(s); len(m) >= 2 {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			return f
		}
	}
	return unknownFloat
}

// aggregateDynamic 对逐卡动态指标做聚合，输出使用率平均值与显存使用率平均值。
func aggregateDynamic(list []GPUDynamicInfo, memTotalByCard map[gpuCardKey]int) GPUDynamicTotal {
	out := GPUDynamicTotal{
		UtilizationAvg:    unknownInt,
		MemUtilizationAvg: unknownInt,
	}
	if len(list) == 0 {
		return out
	}

	sumUtil := 0
	cntUtil := 0
	sumMemUtil := 0.0
	cntMemUtil := 0

	for _, g := range list {
		if g.Utilization != unknownInt {
			sumUtil += g.Utilization
			cntUtil++
		}

		total := memTotalByCard[gpuCardKey{Vendor: g.Vendor, Index: g.Index}]
		if g.MemUsedMB != unknownInt && total > 0 && total != unknownInt {
			sumMemUtil += float64(g.MemUsedMB) / float64(total) * 100
			cntMemUtil++
		}
	}

	if cntUtil > 0 {
		out.UtilizationAvg = int(float64(sumUtil)/float64(cntUtil) + 0.5)
	}
	if cntMemUtil > 0 {
		out.MemUtilizationAvg = int(sumMemUtil/float64(cntMemUtil) + 0.5)
	}
	return out
}
