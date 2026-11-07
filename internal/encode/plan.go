package encode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// Profile holds the output limits. The origin cannot remux or transcode, so these
// describe the one file every client must be able to direct play.
type Profile struct {
	MaxWidth     int
	MaxHeight    int
	VideoKbps    int
	AudioKbps    int
	KeyframeSecs int
	Languages    []string // audio languages to keep; empty keeps all
	Device       string   // VAAPI render node; RKMPP uses fixed device paths
}

// DefaultProfile is the v1 output profile.
func DefaultProfile() Profile {
	return Profile{
		MaxWidth:     1920,
		MaxHeight:    1080,
		VideoKbps:    8000,
		AudioKbps:    160,
		KeyframeSecs: 3,
		Device:       "/dev/dri/renderD128",
	}
}

// Hash identifies the output settings. Device is left out because it picks
// the hardware, not the result.
func (p Profile) Hash() string {
	p.Device = ""
	sum := sha256.Sum256(fmt.Appendf(nil, "%+v", p))
	return hex.EncodeToString(sum[:8])
}

// VideoPlan says how the video stream is produced.
type VideoPlan struct {
	Index   int
	Copy    bool
	Tonemap bool
	Width   int
	Height  int
	Scale   bool
	// SoftwareDecode decodes and scales on the CPU and leaves only the encode
	// to the hardware. It is the retry for a source the hardware decoder
	// rejects, such as HEVC on some Rockchip kernels.
	SoftwareDecode bool
}

// hardwareFrames reports whether frames can stay in hardware memory from the
// decoder to the encoder. Tonemapping and software decoding both need them in
// system memory.
func (v VideoPlan) hardwareFrames() bool {
	return !v.Tonemap && !v.SoftwareDecode
}

// AudioPlan says how one audio stream is produced.
type AudioPlan struct {
	Index int
	Copy  bool
}

// SubPlan is one text subtitle stream to extract to a sidecar.
type SubPlan struct {
	Index  int
	Lang   string
	Styled bool // ASS or SSA, which also keeps a copy with its styling
}

// Plan is the conversion chosen for one source.
type Plan struct {
	Video    VideoPlan
	Audio    []AudioPlan
	Subs     []SubPlan
	Skipped  []string // streams left out, recorded on the job
	Duration float64
}

var textSubCodecs = []string{"subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "text"}

// NewPlan decides what to do with each stream. forceEncode rules out a video
// stream copy, for when the copied source is the suspected fault.
func NewPlan(p *Probe, prof Profile, forceEncode bool) (*Plan, error) {
	v := p.Video()
	if v == nil {
		return nil, errors.New("plan: no video stream")
	}
	if v.Width <= 0 || v.Height <= 0 {
		return nil, errors.New("plan: video stream has no dimensions")
	}
	if p.Duration() <= 0 {
		return nil, errors.New("plan: source has no duration")
	}

	plan := &Plan{Duration: p.Duration()}
	w, h := fitDimensions(v.Width, v.Height, prof.MaxWidth, prof.MaxHeight)
	plan.Video = VideoPlan{
		Index:   v.Index,
		Copy:    !forceEncode && videoCompliant(v, p, prof),
		Tonemap: v.IsHDR(),
		Width:   w,
		Height:  h,
		Scale:   w != v.Width || h != v.Height,
	}

	audio := p.ByType("audio")
	if len(audio) == 0 {
		return nil, errors.New("plan: no audio stream")
	}
	kept := selectAudio(audio, prof.Languages)
	for _, a := range audio {
		if !slices.ContainsFunc(kept, func(k Stream) bool { return k.Index == a.Index }) {
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("audio #%d (%s): language not kept", a.Index, a.Language()))
			continue
		}
		plan.Audio = append(plan.Audio, AudioPlan{
			Index: a.Index,
			Copy:  a.CodecName == "aac" && a.Channels > 0 && a.Channels <= 2,
		})
	}

	for _, s := range p.ByType("subtitle") {
		if slices.Contains(textSubCodecs, s.CodecName) {
			plan.Subs = append(plan.Subs, SubPlan{
				Index:  s.Index,
				Lang:   cleanLang(s.Language()),
				Styled: s.CodecName == "ass" || s.CodecName == "ssa",
			})
			continue
		}
		plan.Skipped = append(plan.Skipped, fmt.Sprintf("subtitle #%d (%s, %s): image subtitles are not served", s.Index, s.CodecName, s.Language()))
	}
	return plan, nil
}

func selectAudio(audio []Stream, languages []string) []Stream {
	if len(languages) == 0 {
		return audio
	}
	var kept []Stream
	for _, a := range audio {
		if slices.Contains(languages, a.Language()) {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		// A silent title is worse than an unwanted language.
		return audio[:1]
	}
	return kept
}

func videoCompliant(v *Stream, p *Probe, prof Profile) bool {
	if v.CodecName != "h264" || v.PixFmt != "yuv420p" || v.IsHDR() {
		return false
	}
	switch v.Profile {
	case "High", "Main", "Constrained Baseline":
	default:
		return false
	}
	if v.Width > prof.MaxWidth || v.Height > prof.MaxHeight {
		return false
	}
	kbps := parseKbps(v.BitRate)
	if kbps == 0 {
		// Containers such as MKV often omit the stream rate. The overall rate
		// includes audio, so this can only err towards re-encoding.
		kbps = parseKbps(p.Format.BitRate)
	}
	return kbps > 0 && kbps <= prof.VideoKbps
}

// fitDimensions scales down to fit the limits, keeping aspect ratio and even
// sizes, which 4:2:0 encoders require.
func fitDimensions(w, h, maxW, maxH int) (int, int) {
	scale := min(1, float64(maxW)/float64(w), float64(maxH)/float64(h))
	return even(float64(w) * scale), even(float64(h) * scale)
}

func even(v float64) int {
	return max(2, int(v+0.5)&^1)
}
