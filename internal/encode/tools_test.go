package encode

import (
	"context"
	"os/exec"
	"testing"
)

func TestHasEncoder(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	if err := HasEncoder(ctx, "ffmpeg", "aac"); err != nil {
		t.Errorf("every ffmpeg has aac: %v", err)
	}
	if err := HasEncoder(ctx, "ffmpeg", "no_such_encoder"); err == nil {
		t.Error("want an error for a missing encoder")
	}
}

func TestTrialEncodeReportsFailure(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if err := TrialEncode(context.Background(), "ffmpeg", "no_such_encoder"); err == nil {
		t.Error("want an error for a missing encoder")
	}
}
