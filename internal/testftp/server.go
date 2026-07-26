// Package testftp runs an in-process FTP server backed by a temp directory.
// It exists so the range proxy and FTP client can be tested against real
// REST/RETR semantics rather than a mock.
package testftp

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

const (
	User = "tester"
	Pass = "hunter2"
)

type driver struct {
	root string
}

func (d *driver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{
		ListenAddr:        "127.0.0.1:0",
		PublicHost:        "127.0.0.1",
		DisableActiveMode: true,
		IdleTimeout:       60,
	}, nil
}

func (d *driver) ClientConnected(ftpserver.ClientContext) (string, error) {
	return "test server", nil
}

func (d *driver) ClientDisconnected(ftpserver.ClientContext) {}

func (d *driver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != User || pass != Pass {
		return nil, errors.New("bad credentials")
	}
	return afero.NewBasePathFs(afero.NewOsFs(), d.root), nil
}

func (d *driver) GetTLSConfig() (*tls.Config, error) {
	return nil, errors.New("TLS not configured")
}

// Start returns the listening address and the served root directory.
func Start(t *testing.T) (addr, root string) {
	t.Helper()

	root = t.TempDir()
	srv := ftpserver.NewFtpServer(&driver{root: root})
	srv.Logger = slog.New(slog.DiscardHandler)

	if err := srv.Listen(); err != nil {
		t.Fatalf("ftp listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Stop() })

	return srv.Addr(), root
}

// WriteFile creates a deterministic pseudo-random file under the served root.
func WriteFile(t *testing.T, root, name string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// Copy places a real file (a media sample, say) under the served root.
func Copy(t *testing.T, root, name, srcPath string) {
	t.Helper()
	b, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
