package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRKMPPDevices(t *testing.T) {
	dev := t.TempDir()
	err := checkRKMPPDevices(dev)
	if err == nil || !strings.Contains(err.Error(), "vendor kernel") {
		t.Fatalf("a missing mpp_service must point at the kernel: %v", err)
	}
	for _, name := range []string{"mpp_service", "rga"} {
		if err := os.WriteFile(filepath.Join(dev, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkRKMPPDevices(dev); err == nil || !strings.Contains(err.Error(), "dri") {
		t.Fatalf("a missing dri must be named: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dev, "dri"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkRKMPPDevices(dev); err != nil {
		t.Fatalf("all devices present: %v", err)
	}
}

func TestHasEncoder(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	if err := hasEncoder(ctx, "ffmpeg", "aac"); err != nil {
		t.Errorf("every ffmpeg has aac: %v", err)
	}
	if err := hasEncoder(ctx, "ffmpeg", "no_such_encoder"); err == nil {
		t.Error("want an error for a missing encoder")
	}
}

func TestTestEncodeReportsFailure(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if err := testEncode(context.Background(), "ffmpeg", "no_such_encoder"); err == nil {
		t.Error("want an error for a missing encoder")
	}
}
