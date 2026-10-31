package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
