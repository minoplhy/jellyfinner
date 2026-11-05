package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"jellyfinner/internal/encode"
	"jellyfinner/internal/job"
	"jellyfinner/internal/transfer"
)

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe", "rclone"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

// makeSource writes a short clip with non-compliant video and audio, a text
// subtitle track, and a second audio track that is already AAC.
func makeSource(t *testing.T, path string) {
	t.Helper()
	srt := filepath.Join(t.TempDir(), "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,500 --> 00:00:01,500\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=4",
		"-i", srt,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3",
		"-c:v", "mpeg4", "-c:a:0", "flac", "-c:a:1", "aac", "-c:s", "srt",
		"-metadata:s:s:0", "language=tha",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make source: %v\n%s", err, out)
	}
}

func testConfig(t *testing.T, backend encode.Backend) Config {
	t.Helper()
	return Config{
		Scratch:  t.TempDir(),
		FFmpeg:   "ffmpeg",
		FFprobe:  "ffprobe",
		Profile:  encode.DefaultProfile(),
		Backend:  backend,
		Transfer: &transfer.Rclone{Binary: "rclone", Streams: 2, Log: &bytes.Buffer{}},
	}
}

func testJob(t *testing.T, source string) *job.Job {
	t.Helper()
	return &job.Job{
		ID:         "testjob",
		Source:     source,
		Incoming:   t.TempDir(),
		OutputPath: "Movies/Test (2026)/Test (2026).mp4",
	}
}

func TestRunEndToEnd(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	makeSource(t, source)
	cfg := testConfig(t, encode.BackendSoftware)
	j := testJob(t, source)

	var stages []string
	manifest, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, func(stage string, _ float64) {
		if len(stages) == 0 || stages[len(stages)-1] != stage {
			stages = append(stages, stage)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(manifest.Files) != 2 {
		t.Fatalf("files = %+v, want the mp4 and one sidecar", manifest.Files)
	}
	dir := filepath.Join(j.Incoming, j.ID)
	for _, f := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(data)) != f.Size {
			t.Errorf("%s does not match its manifest entry", f.Name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Test (2026).tha.srt")); err != nil {
		t.Errorf("sidecar missing: %v", err)
	}

	var pushed Manifest
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.OutputPath != j.OutputPath || pushed.Backend != encode.BackendSoftware {
		t.Errorf("manifest = %+v", pushed)
	}

	probe, err := encode.RunProbe(context.Background(), "ffprobe", filepath.Join(dir, "Test (2026).mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if v := probe.Video(); v == nil || v.CodecName != "h264" {
		t.Errorf("video = %+v", v)
	}
	audio := probe.ByType("audio")
	if len(audio) != 2 || audio[0].Channels != 2 {
		t.Errorf("audio = %+v", audio)
	}

	entries, err := os.ReadDir(cfg.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch not cleared: %v", entries)
	}
	if want := []string{"pull", "encode", "push", "done"}; len(stages) != len(want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

func TestRunKeepsStyledSubtitles(t *testing.T) {
	requireTools(t)
	const styled = `{\an8\c&H00FF00&}hello`
	ass := filepath.Join(t.TempDir(), "s.ass")
	script := "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\n" +
		"Format: Name, Fontname, Fontsize\nStyle: Default,Arial,20\n\n[Events]\n" +
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:00.50,0:00:01.50,Default,,0,0,0,," + styled + "\n"
	if err := os.WriteFile(ass, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.mkv")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-i", ass,
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "mpeg4", "-c:a", "aac", "-c:s", "copy",
		"-metadata:s:s:0", "language=eng",
		source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make source: %v\n%s", err, out)
	}

	j := testJob(t, source)
	if _, err := Run(context.Background(), testConfig(t, encode.BackendSoftware), j, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(j.Incoming, j.ID)
	data, err := os.ReadFile(filepath.Join(dir, "Test (2026).eng.ass"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(styled)) {
		t.Errorf("styling lost: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "Test (2026).eng.srt")); err != nil {
		t.Errorf("plain sidecar missing: %v", err)
	}
}

// The hardware is missing on test machines, so its attempt fails and the job
// must still finish in software.
func TestRunFallsBackToSoftware(t *testing.T) {
	requireTools(t)
	for _, backend := range []encode.Backend{encode.BackendVAAPI, encode.BackendRKMPP} {
		t.Run(string(backend), func(t *testing.T) {
			if backend == encode.BackendRKMPP && hasDevice("/dev/mpp_service") {
				t.Skip("real Rockchip hardware present")
			}
			source := filepath.Join(t.TempDir(), "source.mkv")
			makeSource(t, source)
			cfg := testConfig(t, backend)
			cfg.Profile.Device = "/nonexistent/renderD128"
			j := testJob(t, source)

			log := &bytes.Buffer{}
			manifest, err := Run(context.Background(), cfg, j, log, nil)
			if err != nil {
				t.Fatalf("%v\n%s", err, log)
			}
			if manifest.Backend != encode.BackendSoftware {
				t.Errorf("backend = %q, want software fallback", manifest.Backend)
			}
		})
	}
}

func hasDevice(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRunForceEncodeOverridesCopy(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-movflags", "+faststart", source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make source: %v\n%s", err, out)
	}

	j := testJob(t, source)
	manifest, err := Run(context.Background(), testConfig(t, encode.BackendSoftware), j, &bytes.Buffer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.VideoCopy {
		t.Error("a compliant source should be stream copied")
	}

	j = testJob(t, source)
	j.ForceEncode = true
	manifest, err = Run(context.Background(), testConfig(t, encode.BackendSoftware), j, &bytes.Buffer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.VideoCopy {
		t.Error("ForceEncode must rule out a stream copy")
	}
}

func TestRunTonemapsHDR(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-vf", "format=yuv420p10le", "-c:v", "libx265",
		"-x265-params", "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"-c:a", "flac", source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot make an HDR source: %v\n%s", err, out)
	}
	probe, err := encode.RunProbe(context.Background(), "ffprobe", source)
	if err != nil {
		t.Fatal(err)
	}
	if v := probe.Video(); v == nil || !v.IsHDR() {
		t.Fatalf("test source must be detected as HDR: %+v", v)
	}

	j := testJob(t, source)
	log := &bytes.Buffer{}
	if _, err := Run(context.Background(), testConfig(t, encode.BackendSoftware), j, log, nil); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}

	probe, err = encode.RunProbe(context.Background(), "ffprobe", filepath.Join(j.Incoming, j.ID, "Test (2026).mp4"))
	if err != nil {
		t.Fatal(err)
	}
	v := probe.Video()
	if v == nil || v.IsHDR() || v.ColorTransfer != "bt709" || v.ColorSpace != "bt709" || v.PixFmt != "yuv420p" {
		t.Errorf("output is not SDR BT.709: %+v", v)
	}
}

func TestRunFailureLeavesNoManifest(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(source, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, encode.BackendSoftware)
	j := testJob(t, source)
	if _, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(filepath.Join(j.Incoming, j.ID, ManifestName)); !os.IsNotExist(err) {
		t.Errorf("a failed job must not leave a manifest: %v", err)
	}
}

func TestRetryReusesPulledSource(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	makeSource(t, source)
	cfg := testConfig(t, encode.BackendSoftware)
	cfg.FFmpeg = "false"
	j := testJob(t, source)

	pulled := filepath.Join(cfg.Scratch, j.ID, "source.mkv")
	if _, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want an error")
	}
	first, err := os.Stat(pulled)
	if err != nil {
		t.Fatalf("a failed job must keep its source: %v", err)
	}
	if _, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want an error")
	}
	second, err := os.Stat(pulled)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, second) {
		t.Error("the retry pulled the source again")
	}
}

func TestRunCancelled(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	makeSource(t, source)
	cfg := testConfig(t, encode.BackendSoftware)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, cfg, testJob(t, source), &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want an error")
	}
	entries, err := os.ReadDir(cfg.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch not cleared after cancel: %v", entries)
	}
}

func TestRunShutdownKeepsPulledSource(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	makeSource(t, source)
	cfg := testConfig(t, encode.BackendSoftware)
	j := testJob(t, source)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrShutdown)
	if _, err := Run(ctx, cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(filepath.Join(cfg.Scratch, j.ID)); err != nil {
		t.Errorf("a restart should find its scratch again: %v", err)
	}
}

func TestCheckScratchCountsHeldSource(t *testing.T) {
	const huge = 1 << 62
	if err := checkScratch(t.TempDir(), huge, 0); err == nil {
		t.Error("an impossible size must fail")
	}
	if err := checkScratch(t.TempDir(), 100, 100); err != nil {
		t.Errorf("a held source needs no extra room for itself: %v", err)
	}
}

// failingPush pulls through the real transfer, and fails every push while fail
// is set.
type failingPush struct {
	transfer.Transfer
	fail bool
}

func (f *failingPush) Push(ctx context.Context, j *job.Job, local, name string) error {
	if f.fail {
		return errors.New("upload refused")
	}
	return f.Transfer.Push(ctx, j, local, name)
}

func TestRetryReusesEncode(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	makeSource(t, source)
	cfg := testConfig(t, encode.BackendSoftware)
	flaky := &failingPush{Transfer: cfg.Transfer, fail: true}
	cfg.Transfer = flaky
	j := testJob(t, source)

	if _, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want the push to fail")
	}

	// No working ffmpeg or ffprobe, so only a reused encode can succeed.
	flaky.fail = false
	cfg.FFmpeg, cfg.FFprobe = "false", "false"
	var log bytes.Buffer
	manifest, err := Run(context.Background(), cfg, j, &log, nil)
	if err != nil {
		t.Fatalf("retry encoded again instead of reusing: %v\n%s", err, log.String())
	}
	if len(manifest.Files) != 2 || manifest.Backend != encode.BackendSoftware {
		t.Errorf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(j.Incoming, j.ID, ManifestName)); err != nil {
		t.Errorf("manifest not pushed: %v", err)
	}
}

func TestRetryEncodesAgainWhenSettingsChange(t *testing.T) {
	requireTools(t)
	source := filepath.Join(t.TempDir(), "source.mkv")
	makeSource(t, source)
	cfg := testConfig(t, encode.BackendSoftware)
	flaky := &failingPush{Transfer: cfg.Transfer, fail: true}
	cfg.Transfer = flaky
	j := testJob(t, source)

	if _, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("want the push to fail")
	}

	flaky.fail = false
	cfg.FFmpeg = "false"
	j.ForceEncode = true
	if _, err := Run(context.Background(), cfg, j, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("a changed job must encode again, which fails without ffmpeg")
	}
}
