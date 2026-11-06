package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
// open. The encoder's own node exists only on Rockchip's vendor kernel.
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
	return nil
}
