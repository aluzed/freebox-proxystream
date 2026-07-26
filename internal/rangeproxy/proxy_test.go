package rangeproxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type memSource struct {
	data []byte
}

func (m *memSource) Lookup(ctx context.Context, id int64) (string, int64, error) {
	return "/mem", int64(len(m.data)), nil
}

func (m *memSource) OpenAt(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data[offset:])), nil
}

func newTestServer(t *testing.T, size int) (*httptest.Server, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	srv := httptest.NewServer(New(&memSource{data: data}, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(srv.Close)
	return srv, data
}

// The exact headers below are what make ffmpeg treat the input as seekable.
// If any of them regress, `-ss` silently degrades to sequential decoding.
func TestRangeHeaders(t *testing.T) {
	const size = 10000
	srv, data := newTestServer(t, size)

	cases := []struct {
		name        string
		rangeHeader string
		wantStatus  int
		wantCR      string
		wantLen     string
		wantBody    []byte
	}{
		{"no range", "", http.StatusOK, "", "10000", data},
		{"open ended", "bytes=100-", http.StatusPartialContent, "bytes 100-9999/10000", "9900", data[100:]},
		{"closed", "bytes=100-199", http.StatusPartialContent, "bytes 100-199/10000", "100", data[100:200]},
		{"first byte", "bytes=0-0", http.StatusPartialContent, "bytes 0-0/10000", "1", data[0:1]},
		{"last byte", "bytes=9999-", http.StatusPartialContent, "bytes 9999-9999/10000", "1", data[9999:]},
		{"suffix", "bytes=-500", http.StatusPartialContent, "bytes 9500-9999/10000", "500", data[9500:]},
		{"end clamped to size", "bytes=9000-99999", http.StatusPartialContent, "bytes 9000-9999/10000", "1000", data[9000:]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/raw/1", nil)
			if tc.rangeHeader != "" {
				req.Header.Set("Range", tc.rangeHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
				t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
			}
			if got := resp.Header.Get("Content-Range"); got != tc.wantCR {
				t.Errorf("Content-Range = %q, want %q", got, tc.wantCR)
			}
			if got := resp.Header.Get("Content-Length"); got != tc.wantLen {
				t.Errorf("Content-Length = %q, want %q", got, tc.wantLen)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, tc.wantBody) {
				t.Errorf("body length = %d, want %d (content mismatch=%v)",
					len(body), len(tc.wantBody), !bytes.Equal(body, tc.wantBody))
			}
		})
	}
}

func TestHeadHasSameHeadersAndNoBody(t *testing.T) {
	srv, _ := newTestServer(t, 10000)

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/raw/1", nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 100-199/10000" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "100" {
		t.Errorf("Content-Length = %q, want 100", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned %d body bytes", len(body))
	}
}

func TestUnsatisfiableRanges(t *testing.T) {
	srv, _ := newTestServer(t, 1000)

	for _, h := range []string{"bytes=1000-", "bytes=5000-6000", "bytes=0-10,20-30", "items=0-10", "bytes=abc-"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/raw/1", nil)
		req.Header.Set("Range", h)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("range %q: status = %d, want 416", h, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Range"); got != "bytes */1000" {
			t.Errorf("range %q: Content-Range = %q, want %q", h, got, "bytes */1000")
		}
	}
}
