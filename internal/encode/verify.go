package encode

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
)

const (
	durationTolerance = 1.0 // seconds
	sampleSeconds     = 2.0
)

// Verify checks a finished MP4 before it can be published: streams, duration,
// faststart layout, and a real decode at the start, middle and end.
func Verify(ctx context.Context, ffmpeg, ffprobe, file string, plan *Plan, log io.Writer) error {
	p, err := RunProbe(ctx, ffprobe, file)
	if err != nil {
		return err
	}
	if err := checkOutput(p, plan); err != nil {
		return err
	}
	if err := checkFaststart(file); err != nil {
		return err
	}
	last := max(0, p.Duration()-sampleSeconds-1)
	for _, at := range []float64{0, p.Duration() / 2, last} {
		if err := decodeSample(ctx, ffmpeg, file, at, log); err != nil {
			return fmt.Errorf("verify: decode at %.0fs: %w", at, err)
		}
	}
	return nil
}

func checkOutput(p *Probe, plan *Plan) error {
	if math.Abs(p.Duration()-plan.Duration) > durationTolerance {
		return fmt.Errorf("verify: duration %.2fs, expected %.2fs", p.Duration(), plan.Duration)
	}
	v := p.Video()
	if v == nil {
		return errors.New("verify: no video stream")
	}
	if v.CodecName != "h264" || v.PixFmt != "yuv420p" {
		return fmt.Errorf("verify: video is %s %s, expected h264 yuv420p", v.CodecName, v.PixFmt)
	}
	audio := p.ByType("audio")
	if len(audio) != len(plan.Audio) {
		return fmt.Errorf("verify: %d audio streams, expected %d", len(audio), len(plan.Audio))
	}
	for _, a := range audio {
		if a.CodecName != "aac" {
			return fmt.Errorf("verify: audio stream #%d is %s, expected aac", a.Index, a.CodecName)
		}
	}
	return nil
}

// checkFaststart confirms moov precedes mdat. Without that, clients must fetch
// the end of the file before playback, which is costly over a long link.
func checkFaststart(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	header := make([]byte, 16)
	var offset int64
	for {
		if _, err := f.ReadAt(header[:8], offset); err != nil {
			return fmt.Errorf("verify: no moov atom found: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		switch string(header[4:8]) {
		case "moov":
			return nil
		case "mdat":
			return errors.New("verify: mdat precedes moov, faststart missing")
		}
		if size == 1 {
			if _, err := f.ReadAt(header[8:16], offset+8); err != nil {
				return fmt.Errorf("verify: truncated atom: %w", err)
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
		}
		if size < 8 {
			return fmt.Errorf("verify: invalid atom size %d", size)
		}
		offset += size
	}
}

func decodeSample(ctx context.Context, ffmpeg, file string, at float64, log io.Writer) error {
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-nostdin", "-v", "error", "-xerror",
		"-ss", strconv.FormatFloat(at, 'f', 3, 64),
		"-t", strconv.FormatFloat(sampleSeconds, 'f', 3, 64),
		"-i", file,
		"-map", "0:v:0", "-f", "null", "-")
	cmd.Stderr = log
	return cmd.Run()
}
