package main

import (
	"os"
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

func TestCheckRKMPPDevicesPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	dev := t.TempDir()
	for _, name := range []string{"mpp_service", "rga"} {
		if err := os.WriteFile(filepath.Join(dev, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dev, "dri"), 0o700); err != nil {
		t.Fatal(err)
	}
	heaps := filepath.Join(dev, "dma_heap")
	if err := os.Mkdir(heaps, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(heaps, "system"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	err := checkRKMPPDevices(dev)
	if err == nil || !strings.Contains(err.Error(), "system") || !strings.Contains(err.Error(), "group_add") {
		t.Fatalf("an unopenable heap must be named with the group_add fix: %v", err)
	}
	if err := os.Chmod(filepath.Join(dev, "rga"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := checkRKMPPDevices(dev); err == nil || !strings.Contains(err.Error(), "rga") {
		t.Fatalf("an unopenable rga must be named first: %v", err)
	}
}
