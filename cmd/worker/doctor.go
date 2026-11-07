package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/control"
	"jellyfinner/internal/doctor"
	"jellyfinner/internal/encode"
)

const minFreeGB = 10

// runDoctor keeps going after a failure, so one run shows everything wrong.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	set := flags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := doctor.New(os.Stdout)
	r.Check("scratch writable at "+*set.scratch, doctor.Writable(*set.scratch))
	r.Check("free space", doctor.FreeSpace(*set.scratch, minFreeGB))
	r.Check("ffmpeg", encode.CheckBinary(ctx, *set.ffmpeg))
	r.Check("ffprobe", encode.CheckBinary(ctx, *set.ffprobe))
	if encode.Backend(*set.backend) == encode.BackendVAAPI {
		device := *set.device
		if device == "" {
			device = encode.DefaultProfile().Device
		}
		_, err := os.Stat(device)
		r.Check("GPU at "+device, err)
	}
	if encode.Backend(*set.backend) == encode.BackendRKMPP {
		r.Check("Rockchip devices in /dev", checkRKMPPDevices("/dev"))
		r.Check("chip readable from the device tree", checkDeviceTree("/proc/device-tree/compatible"))
		r.Check("ffmpeg has h264_rkmpp", encode.HasEncoder(ctx, *set.ffmpeg, "h264_rkmpp"))
		r.Check("test encode with h264_rkmpp", encode.TrialEncode(ctx, *set.ffmpeg, "h264_rkmpp"))
	}

	info := gatherInfo(ctx, encode.Backend(*set.backend), *set.device, *set.ffmpeg)
	r.Note(buildinfo.Line("worker"))
	r.Note(fmt.Sprintf("CPU: %s, %d cores, %d threads", info.CPU, info.Cores, info.Threads))
	if info.CPULimit > 0 {
		r.Note(fmt.Sprintf("container CPU limit: %g CPUs", info.CPULimit))
	}
	if info.FFmpeg != "" {
		r.Note(info.FFmpeg)
	}

	client, err := control.NewClient(*set.url, os.Getenv("NODE_TOKEN"))
	r.Check("orchestrator URL and token set", err)
	if err == nil {
		client.Role = "worker"
		me, err := client.WhoAmI(ctx)
		r.Check("orchestrator reachable and token accepted", err)
		if err == nil && me.Kind != "worker" {
			r.Check("node kind", fmt.Errorf("%s is a %s, not a worker", me.Name, me.Kind))
		} else if err == nil {
			r.Note("this node is " + me.Name)
		}
	}
	return r.Done()
}

// checkRKMPPDevices looks for the device nodes the Rockchip encoder and scaler
// open, and opens them the way ffmpeg will. The encoder's own node exists only
// on Rockchip's vendor kernel. A node that is present but not openable is the
// usual cause of a hardware job quietly falling back to software.
func checkRKMPPDevices(dev string) error {
	if _, err := os.Stat(filepath.Join(dev, "mpp_service")); err != nil {
		return fmt.Errorf("%s is missing. RKMPP needs Rockchip's vendor kernel (BSP), because mainline kernels lack MPP, and the device must be passed into the container",
			filepath.Join(dev, "mpp_service"))
	}
	for _, name := range []string{"rga", "dri"} {
		if _, err := os.Stat(filepath.Join(dev, name)); err != nil {
			return fmt.Errorf("%s is missing. Pass it into the container", filepath.Join(dev, name))
		}
	}
	nodes := []string{filepath.Join(dev, "mpp_service"), filepath.Join(dev, "rga")}
	// Optional: only newer vendor kernels have it, but when present, MPP and
	// the RGA allocate their buffers from it.
	heaps, _ := filepath.Glob(filepath.Join(dev, "dma_heap", "*"))
	for _, path := range append(nodes, heaps...) {
		if err := openDevice(path); err != nil {
			return err
		}
	}
	return nil
}

// checkDeviceTree checks that MPP can name the chip. It reads the device tree
// to choose each codec's hardware block, and without it a codec such as HEVC
// on the RK3576 fails while H.264 still works. Docker hides /sys/firmware, which
// /proc/device-tree points into, from every container by default.
func checkDeviceTree(compatible string) error {
	data, err := os.ReadFile(compatible)
	if err != nil {
		return fmt.Errorf("cannot read %s, so MPP cannot tell which chip this is and some codecs fail to open. Add \"systempaths=unconfined\" to the worker's security_opt: %w", compatible, err)
	}
	if !strings.Contains(string(data), "rockchip,") {
		return fmt.Errorf("%s names no Rockchip chip: %q", compatible, strings.ReplaceAll(strings.TrimRight(string(data), "\x00"), "\x00", " "))
	}
	return nil
}

// openDevice opens a device node read-write, as the encoder does. On a
// permission error it names the node's group, which is what group_add needs.
func openDevice(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		return f.Close()
	}
	if !errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	if gid, ok := deviceGroup(path); ok && gid == 0 {
		// Adding root's group would grant far more than the device.
		return fmt.Errorf("cannot open %s: permission denied. It belongs to group root, so no group_add can open it safely. On the host, give it to the video group (chgrp video, mode 660), and add a udev rule so that lasts after a reboot", path)
	} else if ok {
		return fmt.Errorf("cannot open %s: permission denied. It belongs to group %d (usually video on the host), so add %q to the worker's group_add", path, gid, strconv.Itoa(gid))
	}
	return fmt.Errorf("cannot open %s: permission denied. Add the group that owns it (usually video) to the worker's group_add", path)
}

func deviceGroup(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Gid), true
}
