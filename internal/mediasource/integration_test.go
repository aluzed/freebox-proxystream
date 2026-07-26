package mediasource_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/mediasource"
	"github.com/aluzed/freebox-proxystream/internal/rangeproxy"
	"github.com/aluzed/freebox-proxystream/internal/testftp"
)

// End to end: HTTP Range request -> FTP REST/RETR -> bytes, with no local copy.
func TestRangeProxyOverRealFTP(t *testing.T) {
	addr, root := testftp.Start(t)
	const size = 3 << 20
	data := testftp.WriteFile(t, root, "Films/Sample (2021).mkv", size)

	sdb, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	store := db.NewStore(sdb)

	ctx := context.Background()
	lib, err := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/Sample (2021).mkv", size, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	ftp := ftpclient.New(ftpclient.Config{Host: addr, User: testftp.User, Pass: testftp.Pass},
		slog.New(slog.DiscardHandler))
	defer ftp.Close()

	srv := httptest.NewServer(rangeproxy.New(mediasource.New(store, ftp), slog.New(slog.DiscardHandler)).Handler())
	defer srv.Close()

	url := fmt.Sprintf("%s/raw/%d", srv.URL, mediaID)

	// Jump around the file the way ffmpeg does when it parses an index.
	for _, r := range []struct{ start, end int64 }{
		{0, 8191},
		{size - 65536, size - 1},
		{1 << 20, 1<<20 + 4095},
		{42, 42},
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.start, r.end))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("range %d-%d: status %d", r.start, r.end, resp.StatusCode)
		}
		want := data[r.start : r.end+1]
		if !bytes.Equal(body, want) {
			t.Errorf("range %d-%d: got %d bytes, want %d (equal=%v)",
				r.start, r.end, len(body), len(want), bytes.Equal(body, want))
		}
		if got, want := resp.Header.Get("Content-Range"),
			fmt.Sprintf("bytes %d-%d/%d", r.start, r.end, size); got != want {
			t.Errorf("Content-Range = %q, want %q", got, want)
		}
	}

	// A full read must also work and must report the exact size.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ContentLength != size {
		t.Errorf("full GET Content-Length = %d, want %d", resp.ContentLength, size)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if n != size {
		t.Errorf("full GET body = %d bytes, want %d", n, size)
	}
}

func TestRangeProxyUnknownMedia(t *testing.T) {
	sdb, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()

	addr, _ := testftp.Start(t)
	ftp := ftpclient.New(ftpclient.Config{Host: addr, User: testftp.User, Pass: testftp.Pass},
		slog.New(slog.DiscardHandler))
	defer ftp.Close()

	srv := httptest.NewServer(rangeproxy.New(
		mediasource.New(db.NewStore(sdb), ftp), slog.New(slog.DiscardHandler)).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/raw/999")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
