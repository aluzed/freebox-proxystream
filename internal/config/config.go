package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Quality is one selectable output rendition. The project deliberately offers
// two and only two: 720p for hostile networks, 1080p when the link allows it.
// Anything above 1080p is out of scope.
type Quality struct {
	Height  int    `json:"height"`
	Label   string `json:"label"`
	Maxrate string `json:"maxrate"`
	Bufsize string `json:"bufsize"`
}

type Config struct {
	FTPHost    string
	FTPUser    string
	FTPPass    string
	FTPTimeout time.Duration

	HTTPAddr     string
	InternalAddr string

	SessionTTL time.Duration
	// Optional shared bearer token for scripting. Empty disables it.
	APIToken string

	DBPath        string
	CacheDir      string
	SubtitleDir   string
	ThumbDir      string
	CacheMaxBytes int64
	CacheTTL      time.Duration

	// PreferredLang picks the default audio track, as an ISO 639-1 code.
	PreferredLang string

	FFmpegBin       string
	FFprobeBin      string
	VideoEncoder    string
	VAAPIDevice     string
	SegmentDuration int
	X264Preset      string
	X264CRF         int
	AudioBitrate    string

	DefaultHeight int
	Qualities     []Quality

	EncoderReadaheadSegments int
	EncoderIdleTimeout       time.Duration
	MaxConcurrentEncoders    int
}

// InternalBaseURL is what ffmpeg and ffprobe are pointed at. Always loopback.
func (c *Config) InternalBaseURL() string {
	addr := c.InternalAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

func (c *Config) RawURL(mediaID int64) string {
	return fmt.Sprintf("%s/raw/%d", c.InternalBaseURL(), mediaID)
}

// Quality resolves a requested height to a configured rendition, falling back
// to the default. Requests for anything else are silently clamped rather than
// rejected, so a stale bookmark still plays.
func (c *Config) Quality(height int) Quality {
	for _, q := range c.Qualities {
		if q.Height == height {
			return q
		}
	}
	for _, q := range c.Qualities {
		if q.Height == c.DefaultHeight {
			return q
		}
	}
	return c.Qualities[0]
}

func Load(envFiles ...string) (*Config, error) {
	for _, f := range envFiles {
		if _, err := os.Stat(f); err == nil {
			if err := godotenv.Load(f); err != nil {
				return nil, fmt.Errorf("load %s: %w", f, err)
			}
		}
	}

	c := &Config{
		FTPHost:    str("FTP_HOST", "mafreebox.freebox.fr:21"),
		FTPUser:    str("FTP_USER", ""),
		FTPPass:    str("FTP_PASS", ""),
		FTPTimeout: dur("FTP_TIMEOUT", 30*time.Second),

		HTTPAddr:     str("HTTP_ADDR", "0.0.0.0:8080"),
		InternalAddr: str("INTERNAL_ADDR", "127.0.0.1:8081"),

		SessionTTL: dur("SESSION_TTL", 720*time.Hour),
		APIToken:   str("API_TOKEN", ""),

		DBPath:        str("DB_PATH", "./data/app.db"),
		CacheDir:      str("CACHE_DIR", "./data/segments"),
		SubtitleDir:   str("SUBTITLE_DIR", "./data/subs"),
		ThumbDir:      str("THUMB_DIR", "./data/thumbs"),
		CacheMaxBytes: i64("CACHE_MAX_BYTES", 50<<30),
		CacheTTL:      dur("CACHE_TTL", 336*time.Hour),

		PreferredLang: str("PREFERRED_LANG", "fr"),

		FFmpegBin:       str("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:      str("FFPROBE_BIN", "ffprobe"),
		VideoEncoder:    str("VIDEO_ENCODER", "libx264"),
		VAAPIDevice:     str("VAAPI_DEVICE", "/dev/dri/renderD128"),
		SegmentDuration: num("SEGMENT_DURATION", 4),
		X264Preset:      str("X264_PRESET", "veryfast"),
		X264CRF:         num("X264_CRF", 23),
		AudioBitrate:    str("AUDIO_BITRATE", "128k"),

		DefaultHeight: num("DEFAULT_HEIGHT", 720),
		Qualities: []Quality{
			{
				Height:  720,
				Label:   "720p",
				Maxrate: str("VIDEO_MAXRATE_720", "3M"),
				Bufsize: str("VIDEO_BUFSIZE_720", "6M"),
			},
			{
				Height:  1080,
				Label:   "1080p",
				Maxrate: str("VIDEO_MAXRATE_1080", "6M"),
				Bufsize: str("VIDEO_BUFSIZE_1080", "12M"),
			},
		},

		EncoderReadaheadSegments: num("ENCODER_READAHEAD_SEGMENTS", 15),
		EncoderIdleTimeout:       dur("ENCODER_IDLE_TIMEOUT", 90*time.Second),
		MaxConcurrentEncoders:    num("MAX_CONCURRENT_ENCODERS", 2),
	}

	if c.SegmentDuration <= 0 {
		return nil, fmt.Errorf("SEGMENT_DURATION must be > 0")
	}
	if c.MaxConcurrentEncoders <= 0 {
		return nil, fmt.Errorf("MAX_CONCURRENT_ENCODERS must be > 0")
	}
	if c.FTPHost == "" {
		return nil, fmt.Errorf("FTP_HOST is required")
	}
	if !strings.Contains(c.FTPHost, ":") {
		c.FTPHost += ":21"
	}
	if c.DefaultHeight != 720 && c.DefaultHeight != 1080 {
		return nil, fmt.Errorf("DEFAULT_HEIGHT must be 720 or 1080, got %d", c.DefaultHeight)
	}
	return c, nil
}

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func num(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func i64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

func dur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}
