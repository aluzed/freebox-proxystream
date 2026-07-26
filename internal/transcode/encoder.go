package transcode

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/apenombre/freebox-proxystream/internal/config"
)

// Encoder describes how to drive one H.264 encoder. They differ in more than
// a codec name: rate control is not portable (`-crf` exists only on x264,
// `-global_quality` on QSV, `-cq` on NVENC), and VAAPI has to scale on the GPU,
// which changes the shape of the filter chain. Each encoder therefore owns its
// own arguments rather than sharing one template with a substituted name.
type Encoder struct {
	// Name is the ffmpeg encoder, e.g. "h264_videotoolbox".
	Name string
	// Tag goes into the variant key. Segments produced by different encoders
	// must never share a cache directory: same settings, different bitstream.
	Tag string
	// Hardware encoders offload to a fixed-function block. They are far
	// faster and noticeably worse at equal bitrate, which is the trade a
	// small board makes to keep up at all.
	Hardware bool
	// Description is shown when the configured encoder is unavailable.
	Description string

	// inputArgs go before -i, for encoders needing a device opened first.
	inputArgs func(cfg *config.Config) []string
	// filter builds the -vf chain for a target height.
	filter func(cfg *config.Config, height int) string
	// codecArgs builds the codec and rate-control arguments.
	codecArgs func(cfg *config.Config, v Variant) []string
}

// Software x264: the default, and the only one with quality-targeted rate
// control worth the name.
var encoderX264 = Encoder{
	Name: "libx264", Tag: "x264",
	Description: "software x264, works everywhere, needs a capable CPU",
	filter:      scaleSoftware,
	codecArgs: func(cfg *config.Config, v Variant) []string {
		return []string{
			"-c:v", "libx264",
			"-preset", cfg.X264Preset,
			"-crf", strconv.Itoa(cfg.X264CRF),
			"-maxrate", v.Maxrate, "-bufsize", v.Bufsize,
			// 10-bit sources would otherwise encode to High10, which no
			// browser decodes.
			"-pix_fmt", "yuv420p",
			"-profile:v", "high", "-level", "4.0",
			// x264-private: a scene cut would otherwise insert a keyframe off
			// the segment grid.
			"-sc_threshold", "0",
		}
	},
}

// Apple Silicon and Intel Macs. No -crf and no presets: bitrate targeted only.
var encoderVideoToolbox = Encoder{
	Name: "h264_videotoolbox", Tag: "vt", Hardware: true,
	Description: "Apple VideoToolbox, macOS and Apple Silicon",
	filter:      scaleSoftware,
	codecArgs: func(cfg *config.Config, v Variant) []string {
		return []string{
			"-c:v", "h264_videotoolbox",
			"-b:v", v.Maxrate, "-maxrate", v.Maxrate, "-bufsize", v.Bufsize,
			"-pix_fmt", "yuv420p",
			"-profile:v", "high",
			// Let it use as much time as it needs; this is not a live stream.
			"-realtime", "false",
		}
	},
}

// Intel Quick Sync. -global_quality is its ICQ knob, roughly comparable to crf.
var encoderQSV = Encoder{
	Name: "h264_qsv", Tag: "qsv", Hardware: true,
	Description: "Intel Quick Sync, needs /dev/dri and a supported iGPU",
	filter:      scaleSoftware,
	codecArgs: func(cfg *config.Config, v Variant) []string {
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "veryfast",
			"-global_quality", strconv.Itoa(cfg.X264CRF),
			"-maxrate", v.Maxrate, "-bufsize", v.Bufsize,
			"-pix_fmt", "nv12",
			"-profile:v", "high",
		}
	},
}

// NVIDIA. Presets are p1..p7, and -cq is its constant-quality knob.
var encoderNVENC = Encoder{
	Name: "h264_nvenc", Tag: "nvenc", Hardware: true,
	Description: "NVIDIA NVENC, needs an NVIDIA GPU and driver",
	filter:      scaleSoftware,
	codecArgs: func(cfg *config.Config, v Variant) []string {
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p4", "-rc", "vbr",
			"-cq", strconv.Itoa(cfg.X264CRF),
			"-maxrate", v.Maxrate, "-bufsize", v.Bufsize,
			"-pix_fmt", "yuv420p",
			"-profile:v", "high",
		}
	},
}

// VAAPI is the awkward one: frames must be uploaded to the GPU and scaled
// there, so it replaces the filter chain rather than reusing scale=.
var encoderVAAPI = Encoder{
	Name: "h264_vaapi", Tag: "vaapi", Hardware: true,
	Description: "VAAPI, Linux with /dev/dri (Intel or AMD)",
	inputArgs: func(cfg *config.Config) []string {
		return []string{"-vaapi_device", cfg.VAAPIDevice}
	},
	filter: func(cfg *config.Config, height int) string {
		// Upload first, then scale on the GPU. Doing it the other way round
		// scales in software and throws away most of the benefit.
		return fmt.Sprintf("format=nv12,hwupload,scale_vaapi=w=-2:h=%d", height)
	},
	codecArgs: func(cfg *config.Config, v Variant) []string {
		return []string{
			"-c:v", "h264_vaapi",
			"-b:v", v.Maxrate, "-maxrate", v.Maxrate, "-bufsize", v.Bufsize,
			"-profile:v", "high",
		}
	},
}

// Raspberry Pi 3 and 4. The whole reason those boards can do this at all.
var encoderV4L2M2M = Encoder{
	Name: "h264_v4l2m2m", Tag: "v4l2", Hardware: true,
	Description: "Raspberry Pi hardware encoder (Pi 3 and Pi 4; the Pi 5 has none)",
	filter:      scaleSoftware,
	codecArgs: func(cfg *config.Config, v Variant) []string {
		return []string{
			"-c:v", "h264_v4l2m2m",
			"-b:v", v.Maxrate, "-maxrate", v.Maxrate, "-bufsize", v.Bufsize,
			"-pix_fmt", "yuv420p",
			// No -profile:v or -level here: the driver rejects some
			// combinations and the defaults are already browser-safe.
		}
	},
}

func scaleSoftware(cfg *config.Config, height int) string {
	return fmt.Sprintf("scale=-2:%d", height)
}

var encoders = map[string]Encoder{
	encoderX264.Name:         encoderX264,
	encoderVideoToolbox.Name: encoderVideoToolbox,
	encoderQSV.Name:          encoderQSV,
	encoderNVENC.Name:        encoderNVENC,
	encoderVAAPI.Name:        encoderVAAPI,
	encoderV4L2M2M.Name:      encoderV4L2M2M,
}

// LookupEncoder resolves a configured name.
func LookupEncoder(name string) (Encoder, error) {
	if name == "" {
		return encoderX264, nil
	}
	e, ok := encoders[name]
	if !ok {
		return Encoder{}, fmt.Errorf("unknown VIDEO_ENCODER %q, choose one of: %s",
			name, strings.Join(EncoderNames(), ", "))
	}
	return e, nil
}

func EncoderNames() []string {
	out := make([]string, 0, len(encoders))
	for name := range encoders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// VerifyEncoder checks the encoder is actually compiled into this ffmpeg. A
// missing encoder must fail at startup with an explanation, not halfway
// through a film with an opaque exit code.
func VerifyEncoder(ctx context.Context, ffmpegBin string, e Encoder) error {
	out, err := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders").Output()
	if err != nil {
		return fmt.Errorf("could not list encoders from %s: %w", ffmpegBin, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == e.Name {
			return nil
		}
	}
	return fmt.Errorf(
		"VIDEO_ENCODER=%s (%s) is not available in %s; "+
			"available H.264 encoders here: %s",
		e.Name, e.Description, ffmpegBin, strings.Join(availableH264(string(out)), ", "))
}

func availableH264(encoderList string) []string {
	var out []string
	for line := range strings.SplitSeq(encoderList, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(fields[1], "264") {
			out = append(out, fields[1])
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}
