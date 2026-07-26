// Package mediasource binds the media table to the FTP client so the range
// proxy can resolve a media id to a remote byte stream.
package mediasource

import (
	"context"
	"io"

	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
)

type Source struct {
	store *db.Store
	ftp   *ftpclient.Client
}

func New(store *db.Store, ftp *ftpclient.Client) *Source {
	return &Source{store: store, ftp: ftp}
}

func (s *Source) Lookup(ctx context.Context, mediaID int64) (string, int64, error) {
	m, err := s.store.GetMedia(ctx, mediaID)
	if err != nil {
		return "", 0, err
	}
	return m.FTPPath, m.SizeBytes, nil
}

func (s *Source) OpenAt(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	return s.ftp.OpenAt(ctx, path, offset)
}
