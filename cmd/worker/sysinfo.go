package main

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/encode"
)

// gatherInfo describes this worker for the orchestrator. Anything it cannot
// read is left empty rather than failing the start.
func gatherInfo(ctx context.Context, backend encode.Backend, device, ffmpeg string) *control.WorkerInfo {
	info := &control.WorkerInfo{
		Threads: runtime.NumCPU(),
		Backend: string(backend),
		OS:      runtime.GOOS + "/" + runtime.GOARCH,
	}
	if backend == encode.BackendVAAPI {
		info.Device = device
	}
	cpuinfo := readFile("/proc/cpuinfo")
	info.CPU, info.Cores = parseCPUInfo(cpuinfo)
	if info.CPU == "" {
		// Arm kernels often name no CPU model, but the device tree names the board.
		info.CPU = strings.TrimRight(readFile("/sys/firmware/devicetree/base/model"), "\x00\n ")
	}
	if info.CPU == "" {
		info.CPU = runtime.GOARCH
	}
	info.CPULimit = parseCPUMax(readFile("/sys/fs/cgroup/cpu.max"))
	info.Kernel = strings.TrimSpace(readFile("/proc/sys/kernel/osrelease"))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if v, err := encode.Version(ctx, ffmpeg); err == nil {
		info.FFmpeg = v
	}
	return info
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// parseCPUInfo returns the CPU model and the number of physical cores from
// /proc/cpuinfo. Without core IDs, as on most Arm kernels, each listed
// processor counts as a core.
func parseCPUInfo(text string) (model string, cores int) {
	type core struct{ pkg, id string }
	seen := map[core]bool{}
	processors := 0
	var pkg, id string
	flush := func() {
		if id != "" {
			seen[core{pkg, id}] = true
		}
		pkg, id = "", ""
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			flush()
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "processor":
			processors++
		case "model name", "Hardware":
			if model == "" {
				model = value
			}
		case "physical id":
			pkg = value
		case "core id":
			id = value
		}
	}
	flush()
	if len(seen) > 0 {
		return model, len(seen)
	}
	return model, processors
}

// parseCPUMax reads a cgroup v2 cpu.max, "quota period" or "max period", as
// a number of CPUs. No limit is 0.
func parseCPUMax(text string) float64 {
	fields := strings.Fields(text)
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || period <= 0 {
		return 0
	}
	return quota / period
}
