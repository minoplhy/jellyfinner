package encode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Run executes ffmpeg, writing its stderr to log. If onProgress is set it
// receives completion from 0 to 1, based on the plan duration.
func Run(ctx context.Context, ffmpeg string, args []string, duration float64, log io.Writer, onProgress func(float64)) error {
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Stderr = log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if onProgress == nil || duration <= 0 {
			continue
		}
		if us, ok := progressMicros(scanner.Text()); ok {
			onProgress(min(1, float64(us)/1e6/duration))
		}
	}
	// Drain anything the scanner gave up on, so ffmpeg cannot block on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

func progressMicros(line string) (int64, bool) {
	value, ok := strings.CutPrefix(line, "out_time_us=")
	if !ok {
		return 0, false
	}
	us, err := strconv.ParseInt(value, 10, 64)
	if err != nil || us < 0 {
		return 0, false
	}
	return us, true
}
