package pipeline

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"jellyfinner/internal/encode"
)

// TestHardwareEncode proves a GPU backend on real hardware. It runs only when
// JELLYFINNER_HW names the backend, such as on a self-hosted CI runner with
// the device, and fails if the job fell back to software.
func TestHardwareEncode(t *testing.T) {
	backend := encode.Backend(os.Getenv("JELLYFINNER_HW"))
	if backend == "" {
		t.Skip("set JELLYFINNER_HW=rkmpp or vaapi on a machine with that hardware")
	}
	if backend == encode.BackendSoftware || !encode.ValidBackend(backend) {
		t.Fatalf("JELLYFINNER_HW=%q is not a hardware backend", backend)
	}
	requireTools(t)

	sources := map[string][]string{
		// Most of the library: 10-bit HEVC, so the scaler converts the depth.
		"hevc10": {"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=4",
			"-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-preset", "ultrafast"},
		// HDR takes the CPU tonemap path, then the hardware encoder.
		"hdr": {"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=24:duration=2",
			"-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-preset", "ultrafast",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc"},
	}
	for name, encodeArgs := range sources {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source.mkv")
			args := append([]string{"-hide_banner", "-v", "error", "-y"}, encodeArgs...)
			if out, err := exec.Command("ffmpeg", append(args, source)...).CombinedOutput(); err != nil {
				t.Skipf("cannot make the %s source here: %v\n%s", name, err, out)
			}
			cfg := testConfig(t, backend)
			if device := os.Getenv("VAAPI_DEVICE"); device != "" {
				cfg.Profile.Device = device
			}
			if ff := os.Getenv("FFMPEG"); ff != "" {
				cfg.FFmpeg = ff
			}
			if fp := os.Getenv("FFPROBE"); fp != "" {
				cfg.FFprobe = fp
			}
			j := testJob(t, source)

			log := &bytes.Buffer{}
			manifest, err := Run(context.Background(), cfg, j, log, nil)
			if err != nil {
				t.Fatalf("%v\n%s", err, log)
			}
			if manifest.Backend != backend {
				t.Fatalf("backend = %q, want %q: the hardware failed and software took over\n%s", manifest.Backend, backend, log)
			}
		})
	}
}
