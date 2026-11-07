// Package encode turns a source file into a direct-play MP4: it probes the
// source, plans the conversion, builds ffmpeg arguments and verifies the result.
package encode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// Stream is the subset of ffprobe's stream output the planner needs.
type Stream struct {
	Index         int               `json:"index"`
	CodecType     string            `json:"codec_type"`
	CodecName     string            `json:"codec_name"`
	Profile       string            `json:"profile"`
	PixFmt        string            `json:"pix_fmt"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	ColorTransfer string            `json:"color_transfer"`
	ColorSpace    string            `json:"color_space"`
	Channels      int               `json:"channels"`
	BitRate       string            `json:"bit_rate"`
	StartTime     string            `json:"start_time"`
	Duration      string            `json:"duration"`
	Tags          map[string]string `json:"tags"`
	Disposition   map[string]int    `json:"disposition"`
	SideData      []struct {
		Type string `json:"side_data_type"`
	} `json:"side_data_list"`
}

// Format is the subset of ffprobe's container output the planner needs.
type Format struct {
	Duration string `json:"duration"`
	BitRate  string `json:"bit_rate"`
}

// Probe is parsed ffprobe output.
type Probe struct {
	Streams []Stream `json:"streams"`
	Format  Format   `json:"format"`
}

// ParseProbe decodes `ffprobe -print_format json` output.
func ParseProbe(data []byte) (*Probe, error) {
	var p Probe
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse probe: %w", err)
	}
	return &p, nil
}

// RunProbe runs ffprobe on a local file.
func RunProbe(ctx context.Context, ffprobe, file string) (*Probe, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-print_format", "json",
		"-show_format", "-show_streams",
		"--", file)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w", file, err)
	}
	return ParseProbe(out)
}

// Video returns the main video stream, skipping embedded cover art.
func (p *Probe) Video() *Stream {
	for i := range p.Streams {
		s := &p.Streams[i]
		if s.CodecType == "video" && s.Disposition["attached_pic"] == 0 {
			return s
		}
	}
	return nil
}

// ByType returns every stream of a codec type, in file order.
func (p *Probe) ByType(codecType string) []Stream {
	var out []Stream
	for _, s := range p.Streams {
		if s.CodecType == codecType {
			out = append(out, s)
		}
	}
	return out
}

// Duration returns the container duration in seconds, or 0 if unknown.
func (p *Probe) Duration() float64 {
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil {
		return 0
	}
	return d
}

// span returns when the stream starts and ends, in seconds. ok is false when
// its length is unknown. MKV keeps the length only in a DURATION tag.
func (s *Stream) span() (start, end float64, ok bool) {
	start, _ = strconv.ParseFloat(s.StartTime, 64)
	d, err := strconv.ParseFloat(s.Duration, 64)
	if err != nil || d <= 0 {
		d, ok = parseClock(s.Tags["DURATION"])
		if !ok {
			d, ok = parseClock(s.Tags["DURATION-eng"])
		}
		if !ok {
			return 0, 0, false
		}
	}
	return start, start + d, true
}

// parseClock reads an MKV duration tag such as "00:24:00.024000000".
func parseClock(v string) (float64, bool) {
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(v, "%d:%d:%f", &h, &m, &sec); err != nil {
		return 0, false
	}
	d := float64(h*3600+m*60) + sec
	return d, d > 0
}

// Language returns the stream's language tag, or "und" if it has none.
func (s *Stream) Language() string {
	if lang := s.Tags["language"]; lang != "" {
		return lang
	}
	return "und"
}

// IsHDR reports whether the stream needs tonemapping to SDR.
func (s *Stream) IsHDR() bool {
	switch s.ColorTransfer {
	case "smpte2084", "arib-std-b67":
		return true
	}
	for _, sd := range s.SideData {
		if sd.Type == "DOVI configuration record" {
			return true
		}
	}
	return false
}

func parseKbps(bitsPerSecond string) int {
	n, err := strconv.Atoi(bitsPerSecond)
	if err != nil {
		return 0
	}
	return n / 1000
}
