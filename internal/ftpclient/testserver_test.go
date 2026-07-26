package ftpclient

import (
	"io"
	"log/slog"
	"testing"

	"github.com/aluzed/freebox-proxystream/internal/testftp"
)

func startTestFTP(t *testing.T) (addr, root string) {
	t.Helper()
	return testftp.Start(t)
}

func writeTestFile(t *testing.T, root, name string, size int) []byte {
	t.Helper()
	return testftp.WriteFile(t, root, name, size)
}

func newTestClient(t *testing.T, addr string) *Client {
	t.Helper()
	c := New(Config{Host: addr, User: testftp.User, Pass: testftp.Pass}, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
