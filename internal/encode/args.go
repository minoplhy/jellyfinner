package encode

import (
	"fmt"
	"strconv"
	"strings"
)

// Backend selects the video encoder.
type Backend string

const (
	BackendVAAPI    Backend = "vaapi"
	BackendSoftware Backend = "software"
)

// softwareTonemap converts HDR to BT.709 SDR on the CPU. It needs zimg, which
// jellyfin-ffmpeg includes. It is the one path shared by Intel and AMD, so
// the VAAPI backend uses it too and only the encode runs on the GPU.
const softwareTonemap = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// EncodeArgs builds the ffmpeg arguments that write the MP4. Progress is
// reported on stdout in ffmpeg's key=value format.
func EncodeArgs(src, out string, plan *Plan, prof Profile, backend Backend) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-progress", "pipe:1", "-nostats"}
	if !plan.Video.Copy {
		args = append(args, inputArgs(plan, prof, backend)...)
	}
	args = append(args, "-i", src, "-map", fmt.Sprintf("0:%d", plan.Video.Index))
	for _, a := range plan.Audio {
		args = append(args, "-map", fmt.Sprintf("0:%d", a.Index))
	}
	args = append(args, "-map_chapters", "0", "-sn", "-dn")
	args = append(args, videoArgs(plan, prof, backend)...)
	args = append(args, audioArgs(plan, prof)...)
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

func inputArgs(plan *Plan, prof Profile, backend Backend) []string {
	if backend != BackendVAAPI {
		return nil
	}
	if plan.Video.Tonemap {
		return []string{"-vaapi_device", prof.Device}
	}
	return []string{"-hwaccel", "vaapi", "-hwaccel_device", prof.Device, "-hwaccel_output_format", "vaapi"}
}

func videoArgs(plan *Plan, prof Profile, backend Backend) []string {
	if plan.Video.Copy {
		return []string{"-c:v", "copy"}
	}
	kbps := prof.VideoKbps
	args := []string{
		"-vf", videoFilter(plan, backend),
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", prof.KeyframeSecs),
		"-maxrate", fmt.Sprintf("%dk", kbps),
		"-bufsize", fmt.Sprintf("%dk", kbps*2),
		"-profile:v", "high",
	}
	if backend == BackendVAAPI {
		return append(args, "-c:v", "h264_vaapi", "-rc_mode", "VBR", "-b:v", fmt.Sprintf("%dk", kbps))
	}
	return append(args, "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p")
}

func videoFilter(plan *Plan, backend Backend) string {
	v := plan.Video
	if backend == BackendVAAPI && !v.Tonemap {
		return fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=nv12", v.Width, v.Height)
	}
	var chain []string
	if v.Tonemap {
		chain = append(chain, softwareTonemap)
	}
	if v.Scale {
		chain = append(chain, fmt.Sprintf("scale=%d:%d:flags=lanczos", v.Width, v.Height))
	}
	if backend == BackendVAAPI {
		chain = append(chain, "format=nv12", "hwupload")
	} else {
		chain = append(chain, "format=yuv420p")
	}
	return strings.Join(chain, ",")
}

func audioArgs(plan *Plan, prof Profile) []string {
	var args []string
	for i, a := range plan.Audio {
		idx := strconv.Itoa(i)
		if a.Copy {
			args = append(args, "-c:a:"+idx, "copy")
			continue
		}
		args = append(args,
			"-c:a:"+idx, "aac",
			"-ac:a:"+idx, "2",
			"-b:a:"+idx, fmt.Sprintf("%dk", prof.AudioKbps))
	}
	return args
}

// SidecarName returns the name of a subtitle sidecar. n disambiguates several
// tracks in one language, since Jellyfin matches sidecars on the file stem.
func SidecarName(stem, lang string, n int) string {
	if n == 0 {
		return fmt.Sprintf("%s.%s.srt", stem, lang)
	}
	return fmt.Sprintf("%s.%s.%d.srt", stem, lang, n)
}

// StyledName returns the name of a track's ASS sidecar, given its SRT name.
func StyledName(srt string) string {
	return strings.TrimSuffix(srt, ".srt") + ".ass"
}

// SubtitleArgs builds the ffmpeg arguments that extract text subtitles. outs
// holds one SRT path per plan.Subs entry. Every track gets an SRT, which any
// client can show. Styled tracks also get an untouched ASS copy beside it.
func SubtitleArgs(src string, plan *Plan, outs []string) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-i", src}
	for i, s := range plan.Subs {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Index), "-c:s", "srt", "-f", "srt", outs[i])
		if s.Styled {
			args = append(args, "-map", fmt.Sprintf("0:%d", s.Index), "-c:s", "copy", "-f", "ass", StyledName(outs[i]))
		}
	}
	return args
}
