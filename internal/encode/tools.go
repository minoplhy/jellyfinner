package encode

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CheckBinary confirms an ffmpeg or ffprobe binary runs.
func CheckBinary(ctx context.Context, bin string) error {
	return exec.CommandContext(ctx, bin, "-version").Run()
}

// HasEncoder reports whether this ffmpeg build includes an encoder.
func HasEncoder(ctx context.Context, ffmpeg, name string) error {
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

// TrialEncode runs a one-second encode, so a wrong device mount, permission or
// driver shows up here and not on the first real job.
func TrialEncode(ctx context.Context, ffmpeg, encoder string) error {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-v", "error", "-nostdin",
		"-f", "lavfi", "-i", "testsrc2=size=256x144:rate=24:duration=1",
		"-pix_fmt", "nv12", "-c:v", encoder, "-f", "null", "-").CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("%w: %s", err, lines[len(lines)-1])
	}
	return nil
}

// Version returns the first line of ffmpeg -version, such as
// "ffmpeg version 7.1.1-Jellyfin Copyright ...", trimmed of the copyright.
func Version(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "-hide_banner", "-version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	line, _, _ = strings.Cut(line, " Copyright")
	return strings.TrimSpace(line), nil
}
