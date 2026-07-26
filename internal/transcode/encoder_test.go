package transcode

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

func TestUnknownEncoderIsRejectedWithTheOptions(t *testing.T) {
	_, err := LookupEncoder("h265_magic")
	if err == nil {
		t.Fatal("an unknown encoder was accepted")
	}
	for _, want := range []string{"libx264", "h264_videotoolbox", "h264_v4l2m2m"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not list %s: %v", want, err)
		}
	}
}

func TestEmptyEncoderDefaultsToSoftware(t *testing.T) {
	e, err := LookupEncoder("")
	if err != nil || e.Name != "libx264" {
		t.Errorf("got %q, %v; want libx264", e.Name, err)
	}
}

// Rate control is not portable. Each encoder must emit the flags it actually
// understands, and none of the ones it does not.
func TestEachEncoderUsesItsOwnRateControl(t *testing.T) {
	cfg := testConfig()
	m := &db.Media{Height: intp(2160)}

	cases := []struct {
		encoder string
		must    []string
		mustNot []string
	}{
		{"libx264", []string{"-c:v", "libx264", "-crf", "-preset", "-sc_threshold"}, nil},
		// No -crf and no -preset on VideoToolbox; passing them errors out.
		{"h264_videotoolbox", []string{"-c:v", "h264_videotoolbox", "-b:v"},
			[]string{"-crf", "-sc_threshold", "-global_quality", "-cq"}},
		{"h264_qsv", []string{"-c:v", "h264_qsv", "-global_quality"},
			[]string{"-crf", "-sc_threshold", "-cq"}},
		{"h264_nvenc", []string{"-c:v", "h264_nvenc", "-cq", "-rc"},
			[]string{"-crf", "-sc_threshold", "-global_quality"}},
		{"h264_vaapi", []string{"-c:v", "h264_vaapi", "-vaapi_device", "-b:v"},
			[]string{"-crf", "-sc_threshold"}},
		// The Pi driver rejects some profile/level combinations.
		{"h264_v4l2m2m", []string{"-c:v", "h264_v4l2m2m", "-b:v"},
			[]string{"-crf", "-sc_threshold", "-profile:v", "-level"}},
	}

	for _, tc := range cases {
		t.Run(tc.encoder, func(t *testing.T) {
			cfg.VideoEncoder = tc.encoder
			cfg.VAAPIDevice = "/dev/dri/renderD128"
			v := ResolveVariant(cfg, m, 720, 0)
			if v.Encoder.Name != tc.encoder {
				t.Fatalf("variant resolved to %s", v.Encoder.Name)
			}
			args := buildArgs(cfg, 1, v, 0, "/tmp/seg", "/tmp/tmp")

			for _, want := range tc.must {
				if !slices.Contains(args, want) {
					t.Errorf("missing %s in: %s", want, strings.Join(args, " "))
				}
			}
			for _, unwanted := range tc.mustNot {
				if slices.Contains(args, unwanted) {
					t.Errorf("%s must not be passed to %s", unwanted, tc.encoder)
				}
			}

			// Invariants that hold whatever the encoder.
			for _, want := range []string{"-force_key_frames", "-fps_mode",
				"-output_ts_offset", "-ac", "-hls_segment_type"} {
				if !slices.Contains(args, want) {
					t.Errorf("%s lost for %s", want, tc.encoder)
				}
			}
			// The stereo downmix is what keeps dialogue audible.
			if i := slices.Index(args, "-ac"); i < 0 || args[i+1] != "2" {
				t.Error("the stereo downmix was dropped")
			}
		})
	}
}

// VAAPI scales on the GPU, which changes the filter chain rather than reusing
// the software scaler.
func TestVAAPIUploadsBeforeScaling(t *testing.T) {
	cfg := testConfig()
	cfg.VideoEncoder = "h264_vaapi"
	cfg.VAAPIDevice = "/dev/dri/renderD128"

	v := ResolveVariant(cfg, &db.Media{Height: intp(2160)}, 720, 0)
	args := buildArgs(cfg, 1, v, 0, "/tmp/seg", "/tmp/tmp")

	i := slices.Index(args, "-vf")
	if i < 0 {
		t.Fatal("no filter chain")
	}
	chain := args[i+1]
	if !strings.Contains(chain, "hwupload") || !strings.Contains(chain, "scale_vaapi") {
		t.Fatalf("chain = %q, want an upload then a GPU scale", chain)
	}
	if strings.Index(chain, "hwupload") > strings.Index(chain, "scale_vaapi") {
		t.Errorf("chain = %q: scaling before upload happens in software", chain)
	}
	// The device has to be opened before the input.
	dev := slices.Index(args, "-vaapi_device")
	in := slices.Index(args, "-i")
	if dev < 0 || dev > in {
		t.Error("-vaapi_device must come before -i")
	}
}

// Two encoders at identical settings produce different bitstreams, so their
// segments must never land in the same cache directory.
func TestEncoderIsPartOfTheCacheKey(t *testing.T) {
	cfg := testConfig()
	m := &db.Media{Height: intp(2160)}

	seen := map[string]string{}
	for _, name := range EncoderNames() {
		cfg.VideoEncoder = name
		key := ResolveVariant(cfg, m, 720, 0).Key
		if other, clash := seen[key]; clash {
			t.Errorf("%s and %s share the cache key %q", name, other, key)
		}
		seen[key] = name
	}
	if len(seen) != len(EncoderNames()) {
		t.Errorf("got %d distinct keys for %d encoders", len(seen), len(EncoderNames()))
	}
}

func TestVerifyEncoderRejectsWhatFFmpegLacks(t *testing.T) {
	ctx := context.Background()
	// libx264 is in every sane build.
	if err := VerifyEncoder(ctx, "ffmpeg", encoderX264); err != nil {
		t.Skipf("ffmpeg unavailable or without libx264: %v", err)
	}
	// A Linux-only encoder cannot be present on this machine.
	err := VerifyEncoder(ctx, "ffmpeg", encoderV4L2M2M)
	if err == nil {
		return // running on a Pi, fair enough
	}
	if !strings.Contains(err.Error(), "h264_v4l2m2m") ||
		!strings.Contains(err.Error(), "available") {
		t.Errorf("the error does not explain what is available: %v", err)
	}
}
