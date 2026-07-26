package ftpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

type Entry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	IsDir bool      `json:"is_dir"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

type Config struct {
	Host    string
	User    string
	Pass    string
	Timeout time.Duration
}

// Client is deliberately not a connection pool. One long-lived connection is
// kept for browse/scan, and every byte-range read gets its own short-lived
// connection that is thrown away when the read ends.
type Client struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	browse *ftp.ServerConn
}

func New(cfg Config, log *slog.Logger) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Client{cfg: cfg, log: log}
}

func (c *Client) dial(ctx context.Context) (*ftp.ServerConn, error) {
	var lastErr error
	backoff := 500 * time.Millisecond
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		conn, err := ftp.Dial(c.cfg.Host,
			ftp.DialWithContext(ctx),
			ftp.DialWithTimeout(c.cfg.Timeout),
		)
		if err != nil {
			lastErr = fmt.Errorf("dial %s: %w", c.cfg.Host, err)
			continue
		}
		if err := conn.Login(c.cfg.User, c.cfg.Pass); err != nil {
			conn.Quit()
			// Never surface the password, and the library does not include it
			// in its error, but be explicit about what failed.
			lastErr = fmt.Errorf("ftp login as %q failed: %w", c.cfg.User, err)
			continue
		}
		if err := conn.Type(ftp.TransferTypeBinary); err != nil {
			conn.Quit()
			lastErr = fmt.Errorf("set binary mode: %w", err)
			continue
		}
		return conn, nil
	}
	return nil, lastErr
}

// withBrowse runs fn on the shared browse connection, redialling once if the
// connection turned out to be dead.
func (c *Client) withBrowse(ctx context.Context, fn func(*ftp.ServerConn) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.browse != nil {
		if err := c.browse.NoOp(); err != nil {
			c.browse.Quit()
			c.browse = nil
		}
	}
	if c.browse == nil {
		conn, err := c.dial(ctx)
		if err != nil {
			return err
		}
		c.browse = conn
	}

	err := fn(c.browse)
	if err != nil && isConnError(err) {
		c.browse.Quit()
		c.browse = nil

		conn, derr := c.dial(ctx)
		if derr != nil {
			return errors.Join(err, derr)
		}
		c.browse = conn
		return fn(c.browse)
	}
	return err
}

func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	dir = NormalizePath(dir)
	var out []Entry
	err := c.withBrowse(ctx, func(conn *ftp.ServerConn) error {
		entries, err := conn.List(dir)
		if err != nil {
			return err
		}
		out = out[:0]
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." || e.Name == "" {
				continue
			}
			out = append(out, Entry{
				Name:  e.Name,
				Path:  path.Join(dir, e.Name),
				IsDir: e.Type == ftp.EntryTypeFolder,
				Size:  int64(e.Size),
				Mtime: e.Time,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Stat(ctx context.Context, p string) (int64, time.Time, error) {
	p = NormalizePath(p)
	var size int64
	var mtime time.Time
	err := c.withBrowse(ctx, func(conn *ftp.ServerConn) error {
		s, err := conn.FileSize(p)
		if err != nil {
			return err
		}
		size = s
		// MDTM is optional; a missing mtime is not worth failing a scan over.
		if t, err := conn.GetTime(p); err == nil {
			mtime = t
		}
		return nil
	})
	return size, mtime, err
}

// reader owns its own control connection. Closing it tears the whole
// connection down: after an aborted transfer the control channel cannot be
// trusted, so it is never reused.
type reader struct {
	resp *ftp.Response
	conn *ftp.ServerConn
	once sync.Once
}

func (r *reader) Read(p []byte) (int, error) { return r.resp.Read(p) }

func (r *reader) Close() error {
	var err error
	r.once.Do(func() {
		err = r.resp.Close()
		// Quit can block on a half-dead control connection; the deadline the
		// dial set bounds it.
		_ = r.conn.Quit()
	})
	return err
}

// OpenAt issues REST <offset> + RETR and returns a stream starting at offset.
func (c *Client) OpenAt(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, fmt.Errorf("negative offset %d", offset)
	}
	p = NormalizePath(p)

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := conn.RetrFrom(p, uint64(offset))
	if err != nil {
		conn.Quit()
		return nil, fmt.Errorf("RETR %s at %d: %w", p, offset, err)
	}
	return &reader{resp: resp, conn: conn}, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.browse != nil {
		err := c.browse.Quit()
		c.browse = nil
		return err
	}
	return nil
}

// Ping verifies credentials at startup without holding a connection open.
func (c *Client) Ping(ctx context.Context) error {
	return c.withBrowse(ctx, func(conn *ftp.ServerConn) error { return conn.NoOp() })
}

func NormalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

func isConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"broken pipe", "connection reset", "use of closed", "eof",
		"timeout", "not connected", "i/o timeout", "connection refused",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
