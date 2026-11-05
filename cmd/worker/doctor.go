package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	r.Check("ffmpeg", exec.CommandContext(ctx, *set.ffmpeg, "-version").Run())
	r.Check("ffprobe", exec.CommandContext(ctx, *set.ffprobe, "-version").Run())
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
		r.Check("ffmpeg has h264_rkmpp", hasEncoder(ctx, *set.ffmpeg, "h264_rkmpp"))
		r.Check("test encode with h264_rkmpp", testEncode(ctx, *set.ffmpeg, "h264_rkmpp"))
	}

	client, err := control.NewClient(*set.url, os.Getenv("NODE_TOKEN"))
	r.Check("orchestrator URL and token set", err)
	if err == nil {
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

// hasEncoder reports whether this ffmpeg build includes an encoder.
func hasEncoder(ctx context.Context, ffmpeg, name string) error {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == name {
			return nil
		}
	}
	return fmt.Errorf("%s does not include %s; use jellyfin-ffmpeg, as the worker image does", ffmpeg, name)
}

// testEncode runs a one-second encode, so a wrong device mount, permission or
// driver shows up here and not on the first real job.
func testEncode(ctx context.Context, ffmpeg, encoder string) error {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-v", "error", "-nostdin",
		"-f", "lavfi", "-i", "testsrc2=size=256x144:rate=24:duration=1",
		"-pix_fmt", "nv12", "-c:v", encoder, "-f", "null", "-").CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("%w: %s", err, lines[len(lines)-1])
	}
	return nil
}
