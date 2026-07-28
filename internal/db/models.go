package db

import (
	"encoding/json"
	"time"
)

const (
	StatusNew     = "new"
	StatusProbing = "probing"
	StatusReady   = "ready"
	StatusError   = "error"

	KindMovie = "movie"
	KindTV    = "tv"
)

type Library struct {
	ID        int64      `json:"id"`
	FTPPath   string     `json:"ftp_path"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	CreatedAt time.Time  `json:"created_at"`
	LastScan  *time.Time `json:"last_scan"`
}

// AudioIndex / SubIndex are the per-type indices ffmpeg uses in `-map 0:a:N`
// and `-map 0:s:N`. Index is the absolute stream index, kept for diagnostics.
type AudioStream struct {
	Index      int    `json:"index"`
	AudioIndex int    `json:"audio_index"`
	Codec      string `json:"codec"`
	Lang       string `json:"lang"`
	Channels   int    `json:"channels"`
	Title      string `json:"title"`
}

type SubStream struct {
	Index          int    `json:"index"`
	SubIndex       int    `json:"sub_index"`
	Codec          string `json:"codec"`
	Lang           string `json:"lang"`
	Title          string `json:"title"`
	Forced         bool   `json:"forced"`
	BurnInRequired bool   `json:"burn_in_required"`
}

type Media struct {
	ID        int64      `json:"id"`
	LibraryID int64      `json:"library_id"`
	FTPPath   string     `json:"ftp_path"`
	SizeBytes int64      `json:"size_bytes"`
	FTPMtime  *time.Time `json:"ftp_mtime"`

	ProbedAt     *time.Time    `json:"probed_at"`
	DurationSec  *float64      `json:"duration_sec"`
	VideoCodec   *string       `json:"video_codec"`
	Width        *int          `json:"width"`
	Height       *int          `json:"height"`
	FPS          *float64      `json:"fps"`
	AudioStreams []AudioStream `json:"audio_streams"`
	SubStreams   []SubStream   `json:"sub_streams"`

	// Title and Overview are derived from the path, then owned by the user as
	// soon as they edit them. UserEdited is what protects their words from the
	// next refresh.
	Title      *string `json:"title"`
	Year       *int    `json:"year"`
	Season     *int    `json:"season"`
	Episode    *int    `json:"episode"`
	Overview   *string `json:"overview"`
	UserEdited bool    `json:"user_edited"`

	Status    string    `json:"status"`
	ErrorMsg  *string   `json:"error_msg"`
	CreatedAt time.Time `json:"created_at"`
}

// DisplayTitle falls back to the file name when no metadata was matched.
// Value receiver so templates can call it on slice elements.
func (m Media) DisplayTitle() string {
	if m.Title != nil && *m.Title != "" {
		return *m.Title
	}
	return baseName(m.FTPPath)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalAudio(s *string) []AudioStream {
	out := []AudioStream{}
	if s != nil && *s != "" {
		_ = json.Unmarshal([]byte(*s), &out)
	}
	return out
}

func unmarshalSubs(s *string) []SubStream {
	out := []SubStream{}
	if s != nil && *s != "" {
		_ = json.Unmarshal([]byte(*s), &out)
	}
	return out
}

type Progress struct {
	MediaID     int64     `json:"media_id"`
	PositionSec float64   `json:"position_sec"`
	Watched     bool      `json:"watched"`
	UpdatedAt   time.Time `json:"updated_at"`
}
