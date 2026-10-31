package encode

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func probeOf(streams ...Stream) *Probe {
	return &Probe{Streams: streams, Format: Format{Duration: "100.0", BitRate: "5000000"}}
}

func h264() Stream {
	return Stream{Index: 0, CodecType: "video", CodecName: "h264", Profile: "High", PixFmt: "yuv420p", Width: 1920, Height: 1080}
}

func aac() Stream {
	return Stream{Index: 1, CodecType: "audio", CodecName: "aac", Channels: 2}
}

func TestPlanVideo(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Stream)
		force      bool
		wantCopy   bool
		wantTone   bool
		wantWidth  int
		wantHeight int
	}{
		{"compliant copies", func(*Stream) {}, false, true, false, 1920, 1080},
		{"force encode", func(*Stream) {}, true, false, false, 1920, 1080},
		{"hevc encodes", func(s *Stream) { s.CodecName = "hevc" }, false, false, false, 1920, 1080},
		{"10-bit encodes", func(s *Stream) { s.PixFmt = "yuv420p10le" }, false, false, false, 1920, 1080},
		{"Hi10 encodes", func(s *Stream) { s.Profile = "High 10" }, false, false, false, 1920, 1080},
		{"4K scales", func(s *Stream) { s.Width, s.Height = 3840, 2160 }, false, false, false, 1920, 1080},
		{"scope keeps aspect", func(s *Stream) { s.Width, s.Height = 3840, 1608 }, false, false, false, 1920, 804},
		{"high bitrate encodes", func(s *Stream) { s.BitRate = "20000000" }, false, false, false, 1920, 1080},
		{"PQ tonemaps", func(s *Stream) { s.ColorTransfer = "smpte2084" }, false, false, true, 1920, 1080},
		{"HLG tonemaps", func(s *Stream) { s.ColorTransfer = "arib-std-b67" }, false, false, true, 1920, 1080},
		{"dolby vision tonemaps", func(s *Stream) {
			s.SideData = append(s.SideData, struct {
				Type string `json:"side_data_type"`
			}{"DOVI configuration record"})
		}, false, false, true, 1920, 1080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := h264()
			tt.mutate(&v)
			plan, err := NewPlan(probeOf(v, aac()), DefaultProfile(), tt.force)
			if err != nil {
				t.Fatal(err)
			}
			got := plan.Video
			if got.Copy != tt.wantCopy || got.Tonemap != tt.wantTone || got.Width != tt.wantWidth || got.Height != tt.wantHeight {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestPlanUnknownBitrateEncodes(t *testing.T) {
	p := probeOf(h264(), aac())
	p.Format.BitRate = ""
	plan, err := NewPlan(p, DefaultProfile(), false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Video.Copy {
		t.Error("an unknown bitrate must not be assumed compliant")
	}
}

func TestPlanRejectsUnusableSources(t *testing.T) {
	noDuration := probeOf(h264(), aac())
	noDuration.Format.Duration = ""
	cover := h264()
	cover.Disposition = map[string]int{"attached_pic": 1}
	for name, p := range map[string]*Probe{
		"no video":     probeOf(aac()),
		"no audio":     probeOf(h264()),
		"no duration":  noDuration,
		"only a cover": probeOf(cover, aac()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPlan(p, DefaultProfile(), false); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestPlanAudio(t *testing.T) {
	eng51 := Stream{Index: 1, CodecType: "audio", CodecName: "eac3", Channels: 6, Tags: map[string]string{"language": "eng"}}
	thaAAC := Stream{Index: 2, CodecType: "audio", CodecName: "aac", Channels: 2, Tags: map[string]string{"language": "tha"}}
	aac51 := Stream{Index: 3, CodecType: "audio", CodecName: "aac", Channels: 6, Tags: map[string]string{"language": "jpn"}}

	plan, err := NewPlan(probeOf(h264(), eng51, thaAAC, aac51), DefaultProfile(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []AudioPlan{{1, false}, {2, true}, {3, false}}
	if !slices.Equal(plan.Audio, want) {
		t.Errorf("audio = %+v, want %+v", plan.Audio, want)
	}

	prof := DefaultProfile()
	prof.Languages = []string{"tha"}
	plan, err = NewPlan(probeOf(h264(), eng51, thaAAC), prof, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Audio) != 1 || plan.Audio[0].Index != 2 || len(plan.Skipped) != 1 {
		t.Errorf("language filter: audio %+v, skipped %v", plan.Audio, plan.Skipped)
	}

	prof.Languages = []string{"fra"}
	plan, err = NewPlan(probeOf(h264(), eng51, thaAAC), prof, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Audio) != 1 || plan.Audio[0].Index != 1 {
		t.Errorf("no language match must keep the first track, got %+v", plan.Audio)
	}
}

func TestPlanSubtitles(t *testing.T) {
	text := Stream{Index: 2, CodecType: "subtitle", CodecName: "subrip", Tags: map[string]string{"language": "tha"}}
	image := Stream{Index: 3, CodecType: "subtitle", CodecName: "hdmv_pgs_subtitle"}
	plan, err := NewPlan(probeOf(h264(), aac(), text, image), DefaultProfile(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Subs, []SubPlan{{Index: 2, Lang: "tha"}}) {
		t.Errorf("subs = %+v", plan.Subs)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0], "pgs") {
		t.Errorf("image subtitles must be recorded as skipped, got %v", plan.Skipped)
	}
}

func TestFitDimensionsEven(t *testing.T) {
	tests := []struct{ w, h, wantW, wantH int }{
		{1920, 1080, 1920, 1080},
		{1280, 720, 1280, 720},
		{3840, 2160, 1920, 1080},
		{1920, 1200, 1728, 1080},
		{1440, 1080, 1440, 1080},
		{853, 480, 852, 480},
	}
	for _, tt := range tests {
		w, h := fitDimensions(tt.w, tt.h, 1920, 1080)
		if w != tt.wantW || h != tt.wantH {
			t.Errorf("fit(%d,%d) = %d,%d, want %d,%d", tt.w, tt.h, w, h, tt.wantW, tt.wantH)
		}
	}
}

func join(args []string) string { return strings.Join(args, " ") }

func TestEncodeArgs(t *testing.T) {
	prof := DefaultProfile()
	base := Plan{Audio: []AudioPlan{{Index: 1, Copy: true}, {Index: 2}}, Duration: 10}

	copied := base
	copied.Video = VideoPlan{Copy: true, Width: 1920, Height: 1080}
	got := join(EncodeArgs("in", "out", &copied, prof, BackendVAAPI))
	for _, want := range []string{"-c:v copy", "-c:a:0 copy", "-c:a:1 aac", "-ac:a:1 2", "+faststart", "-map 0:0", "-map 0:1", "-map 0:2"} {
		if !strings.Contains(got, want) {
			t.Errorf("copy args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "hwaccel") {
		t.Errorf("a stream copy must not touch the GPU: %s", got)
	}

	vaapi := base
	vaapi.Video = VideoPlan{Width: 1280, Height: 720, Scale: true}
	got = join(EncodeArgs("in", "out", &vaapi, prof, BackendVAAPI))
	for _, want := range []string{"-hwaccel vaapi", "scale_vaapi=w=1280:h=720:format=nv12", "h264_vaapi", "-rc_mode VBR", "-b:v 8000k"} {
		if !strings.Contains(got, want) {
			t.Errorf("vaapi args missing %q: %s", want, got)
		}
	}

	hdr := base
	hdr.Video = VideoPlan{Tonemap: true, Width: 1920, Height: 1080}
	got = join(EncodeArgs("in", "out", &hdr, prof, BackendVAAPI))
	for _, want := range []string{"-vaapi_device", "tonemap=tonemap=hable", "format=nv12,hwupload", "h264_vaapi"} {
		if !strings.Contains(got, want) {
			t.Errorf("vaapi tonemap args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "-hwaccel ") {
		t.Errorf("software tonemap needs software decode: %s", got)
	}

	soft := base
	soft.Video = VideoPlan{Tonemap: true, Width: 1280, Height: 720, Scale: true}
	got = join(EncodeArgs("in", "out", &soft, prof, BackendSoftware))
	for _, want := range []string{"libx264", "-crf 20", "tonemap=tonemap=hable", "scale=1280:720", "-pix_fmt yuv420p"} {
		if !strings.Contains(got, want) {
			t.Errorf("software args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "vaapi") {
		t.Errorf("the software backend must not reference VAAPI: %s", got)
	}
}

func TestSubtitleArgsKeepStyledCopy(t *testing.T) {
	plan := &Plan{Subs: []SubPlan{{Index: 2, Lang: "jpn"}, {Index: 3, Lang: "eng", Styled: true}}}
	got := join(SubtitleArgs("in", plan, []string{"a.jpn.srt", "a.eng.srt"}))
	for _, want := range []string{"-map 0:2 -c:s srt -f srt a.jpn.srt", "-map 0:3 -c:s srt -f srt a.eng.srt", "-map 0:3 -c:s copy -f ass a.eng.ass"} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "a.jpn.ass") {
		t.Errorf("a plain track must not get an ASS copy: %s", got)
	}
}

func TestSidecarName(t *testing.T) {
	if got := SidecarName("Dune", "tha", 0); got != "Dune.tha.srt" {
		t.Error(got)
	}
	if got := SidecarName("Dune", "tha", 1); got != "Dune.tha.1.srt" {
		t.Error(got)
	}
}

func TestProgressMicros(t *testing.T) {
	if us, ok := progressMicros("out_time_us=1500000"); !ok || us != 1500000 {
		t.Errorf("got %d %v", us, ok)
	}
	for _, line := range []string{"frame=10", "out_time_us=N/A", "out_time_us=-5"} {
		if _, ok := progressMicros(line); ok {
			t.Errorf("%q must not parse", line)
		}
	}
}

func atom(name string, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, uint32(size))
	copy(b[4:], name)
	return b
}

func TestCheckFaststart(t *testing.T) {
	large := make([]byte, 16)
	binary.BigEndian.PutUint32(large, 1)
	copy(large[4:], "free")
	binary.BigEndian.PutUint64(large[8:], 16)

	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{"moov first", slices.Concat(atom("ftyp", 16), atom("moov", 32), atom("mdat", 16)), false},
		{"mdat first", slices.Concat(atom("ftyp", 16), atom("mdat", 32), atom("moov", 16)), true},
		{"64-bit atom", slices.Concat(large, atom("moov", 8)), false},
		{"no moov", atom("ftyp", 16), true},
		{"zero size", atom("ftyp", 16)[:8:8], true},
	}
	tests[4].data = make([]byte, 8)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "x.mp4")
			if err := os.WriteFile(file, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := checkFaststart(file); (err != nil) != tt.wantErr {
				t.Errorf("checkFaststart() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckOutput(t *testing.T) {
	plan := &Plan{Duration: 100, Audio: []AudioPlan{{Index: 1}}}
	if err := checkOutput(probeOf(h264(), aac()), plan); err != nil {
		t.Errorf("good output rejected: %v", err)
	}

	short := probeOf(h264(), aac())
	short.Format.Duration = "90"
	hevc := h264()
	hevc.CodecName = "hevc"
	ac3 := aac()
	ac3.CodecName = "ac3"
	for name, p := range map[string]*Probe{
		"short":       short,
		"wrong codec": probeOf(hevc, aac()),
		"lost audio":  probeOf(h264()),
		"wrong audio": probeOf(h264(), ac3),
	} {
		if err := checkOutput(p, plan); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
