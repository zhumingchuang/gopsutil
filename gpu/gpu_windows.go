// SPDX-License-Identifier: BSD-3-Clause
//go:build windows

package gpu

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type GPUInfo struct {
	Vendor      string  `json:"vendor"` // "nvidia" or "amd"
	Index       int     `json:"index"`
	Name        string  `json:"name"`
	Utilization int     `json:"utilization_percent"`
	MemTotalMB  int     `json:"memory_total_mb"`
	MemUsedMB   int     `json:"memory_used_mb"`
	TempC       int     `json:"temperature_c"`
	PowerW      float64 `json:"power_w"`
}

const unknownInt = -1
const unknownFloat = -1

func HasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func QueryOnce(hasNvidia, hasAmd bool) ([]GPUInfo, error) {
	// Prefer NVIDIA if available (unchanged behavior). Otherwise, AMD.
	if hasNvidia {
		infos, err := queryNvidia()
		if err == nil {
			return infos, nil
		}
		// If nvidia-smi present but failed, return error (likely driver issue).
		return nil, err
	}
	// AMD path
	if hasAmd {
		return queryAmd()
	}
	return nil, fmt.Errorf("no supported GPU query tool available")
}

/* -------- NVIDIA (unchanged) -------- */

func queryNvidia() ([]GPUInfo, error) {
	// fields: index,name,utilization.gpu,memory.total,memory.used,temperature.gpu,power.draw
	args := []string{
		"--query-gpu=index,name,utilization.gpu,memory.total,memory.used,temperature.gpu,power.draw",
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
	var infos []GPUInfo
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitCSVLine(line)
		if len(fields) < 7 {
			return nil, fmt.Errorf("unexpected nvidia-smi output line: %q", line)
		}
		info := GPUInfo{
			Vendor:      "nvidia",
			Utilization: unknownInt,
			MemTotalMB:  unknownInt,
			MemUsedMB:   unknownInt,
			TempC:       unknownInt,
			PowerW:      unknownFloat,
		}
		// parse index
		if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
			info.Index = v
		}
		info.Name = strings.TrimSpace(fields[1])
		if v, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil {
			info.Utilization = v
		}
		if v, err := parseIntRelax(fields[3]); err == nil {
			info.MemTotalMB = v
		}
		if v, err := parseIntRelax(fields[4]); err == nil {
			info.MemUsedMB = v
		}
		if v, err := strconv.Atoi(strings.TrimSpace(fields[5])); err == nil {
			info.TempC = v
		}
		// power.draw may be float like 80.50
		if v, err := strconv.ParseFloat(strings.TrimSpace(fields[6]), 64); err == nil {
			info.PowerW = v
		}
		infos = append(infos, info)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return infos, nil
}

func splitCSVLine(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

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

/* -------- AMD (rocm-smi, heuristic parsing) -------- */

func queryAmd() ([]GPUInfo, error) {
	// We call rocm-smi without special flags and parse its human-readable table output.
	// This parsing is heuristic and attempts to handle common rocm-smi output formats.
	cmd := exec.Command("rocm-smi")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Even if it exits with non-zero, sometimes it prints info; still try to parse.
		// But if there's no output, surface the error.
		if len(out) == 0 {
			if ee, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("rocm-smi failed: %s", strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, err
		}
	}
	text := string(out)
	infos := parseRocmSmiTable(text)
	if len(infos) == 0 {
		// If we couldn't parse anything, return an error with sample output for debugging.
		snippet := text
		if len(snippet) > 800 {
			snippet = snippet[:800] + "..."
		}
		return nil, fmt.Errorf("failed to parse rocm-smi output (output snippet: %q)", snippet)
	}
	// set vendor
	for i := range infos {
		infos[i].Vendor = "amd"
	}
	return infos, nil
}

func parseRocmSmiTable(text string) []GPUInfo {
	scanner := bufio.NewScanner(strings.NewReader(text))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// First pass: collect name lines (patterns like "GPU 0 : Radeon RX ..." or "Card 0 : <name>")
	nameMap := map[int]string{}
	reName1 := regexp.MustCompile(`(?i)^\s*GPU\s*(\d+)\s*[:\-]\s*(.+)$`)
	reName2 := regexp.MustCompile(`(?i)^\s*Card\s*(\d+)\s*[:\-]\s*(.+)$`)
	reName3 := regexp.MustCompile(`(?i)^\s*(\d+)\)\s*(.+)$`) // sometimes list-like
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

	// Second pass: find lines that begin with index or contain "GPU" table lines.
	reIndexLine := regexp.MustCompile(`^\s*(\d+)\s+(.+)$`)

	var infos []GPUInfo
	for _, ln := range lines {
		lnTrim := strings.TrimSpace(ln)
		if lnTrim == "" {
			continue
		}
		// often table rows start with index
		if m := reIndexLine.FindStringSubmatch(ln); len(m) == 3 {
			idx, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			rest := m[2]
			info := GPUInfo{
				Index:       idx,
				Utilization: unknownInt,
				MemTotalMB:  unknownInt,
				MemUsedMB:   unknownInt,
				TempC:       unknownInt,
				PowerW:      unknownFloat,
			}
			// name from nameMap if present
			if n, ok := nameMap[idx]; ok {
				info.Name = n
			}
			// attempt to parse fields from rest
			info.Utilization = findFirstPercent(rest)
			used, total := findMemoryUsedAndTotal(rest)
			info.MemUsedMB = used
			info.MemTotalMB = total
			info.TempC = findTemp(rest)
			info.PowerW = findPower(rest)
			infos = appendOrMerge(infos, info)
		}
		// also match lines like "GPU 0 : ..." already handled in name pass; but some outputs put all metrics in later lines:
		// Some rocm-smi prints lines like "===================", others like "GPU 0: VRAM: 1024/8192 MiB, GPU use: 3%, Temp: 34.0c, Pwr: 10.00W"
		// Try to extract metric-rich lines referring to a specific GPU: look for "GPU <n>" within the line.
		reInline := regexp.MustCompile(`(?i)GPU\s*(\d+)[:\s,]+(.+)`)
		if m := reInline.FindStringSubmatch(ln); len(m) == 3 {
			idx, err := strconv.Atoi(strings.TrimSpace(m[1]))
			if err != nil {
				continue
			}
			rest := m[2]
			info := GPUInfo{
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

	// Sort/compact by index order and return
	// Create map by index to pick highest-quality entry (merge fields)
	byIndex := map[int]GPUInfo{}
	for _, g := range infos {
		if ex, ok := byIndex[g.Index]; ok {
			byIndex[g.Index] = mergeGPUInfo(ex, g)
		} else {
			byIndex[g.Index] = g
		}
	}
	// create slice ordered by index
	var ordered []GPUInfo
	for _, g := range byIndex {
		ordered = append(ordered, g)
	}
	// sort by index (simple insertion sort since GPU count small)
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].Index < ordered[i].Index {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	return ordered
}

func appendOrMerge(list []GPUInfo, g GPUInfo) []GPUInfo {
	for i := range list {
		if list[i].Index == g.Index {
			list[i] = mergeGPUInfo(list[i], g)
			return list
		}
	}
	return append(list, g)
}

func mergeGPUInfo(a, b GPUInfo) GPUInfo {
	// prefer non-empty fields from b, else keep a
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

func findMemoryUsedAndTotal(s string) (used, total int) {
	// try named VRAM pattern first
	if m := reMemNamed.FindStringSubmatch(s); len(m) == 3 {
		if u, err := strconv.Atoi(m[1]); err == nil {
			used = u
		}
		if t, err := strconv.Atoi(m[2]); err == nil {
			total = t
		}
		return
	}
	// generic slash pattern
	if m := reMemSlash.FindStringSubmatch(s); len(m) == 3 {
		if u, err := strconv.Atoi(m[1]); err == nil {
			used = u
		}
		if t, err := strconv.Atoi(m[2]); err == nil {
			total = t
		}
		return
	}
	// nothing
	return unknownInt, unknownInt
}

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

func findPower(s string) float64 {
	if m := rePowerW.FindStringSubmatch(s); len(m) >= 2 {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			return f
		}
	}
	return unknownFloat
}

/* -------- Output -------- */

func printInfos(infos []GPUInfo, jsonFlag bool) {
	if jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(infos); err != nil {
			fmt.Fprintln(os.Stderr, "failed to encode json:", err)
		}
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("# GPU info @ %s\n", now)
	fmt.Printf("%-6s  %-3s  %-30s  %4s  %8s  %8s  %4s  %6s\n",
		"VENDOR", "ID", "NAME", "GPU%", "MEM_TOT", "MEM_USE", "TMP", "PWR(W)")
	for _, g := range infos {
		name := truncate(g.Name, 30)
		fmt.Printf("%-6s  %-3d  %-30s  %4d%%  %8d  %8d  %4dC  %6.2f\n",
			strings.ToUpper(g.Vendor), g.Index, name, g.Utilization, g.MemTotalMB, g.MemUsedMB, g.TempC, g.PowerW)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
