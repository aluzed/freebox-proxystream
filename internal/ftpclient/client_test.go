package ftpclient

import (
	"bytes"
	"context"
	"testing"
)

func TestOpenAtOffsets(t *testing.T) {
	addr, root := startTestFTP(t)
	data := writeTestFile(t, root, "Films/big.mkv", 1<<20)
	c := newTestClient(t, addr)
	ctx := context.Background()

	for _, offset := range []int64{0, 1, 4096, 65536, 1<<20 - 1} {
		rc, err := c.OpenAt(ctx, "/Films/big.mkv", offset)
		if err != nil {
			t.Fatalf("OpenAt(%d): %v", offset, err)
		}
		got := readAll(t, rc)
		want := data[offset:]
		if !bytes.Equal(got, want) {
			t.Errorf("OpenAt(%d): got %d bytes, want %d", offset, len(got), len(want))
		}
	}
}

// Closing a reader early is the aborted-transfer case. The next read must still
// work, which is the whole reason a fresh connection is dialled every time.
func TestAbortedTransferDoesNotPoisonNextRead(t *testing.T) {
	addr, root := startTestFTP(t)
	data := writeTestFile(t, root, "big.mkv", 1<<20)
	c := newTestClient(t, addr)
	ctx := context.Background()

	for range 3 {
		rc, err := c.OpenAt(ctx, "/big.mkv", 0)
		if err != nil {
			t.Fatalf("OpenAt: %v", err)
		}
		buf := make([]byte, 1024)
		if _, err := rc.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		rc.Close() // abort mid-transfer
	}

	rc, err := c.OpenAt(ctx, "/big.mkv", 512)
	if err != nil {
		t.Fatalf("OpenAt after aborts: %v", err)
	}
	got := readAll(t, rc)
	if !bytes.Equal(got, data[512:]) {
		t.Errorf("got %d bytes after aborted transfers, want %d", len(got), len(data)-512)
	}
}

func TestListAndStat(t *testing.T) {
	addr, root := startTestFTP(t)
	writeTestFile(t, root, "Films/a.mkv", 2048)
	writeTestFile(t, root, "Films/Sub/b.mp4", 4096)
	c := newTestClient(t, addr)
	ctx := context.Background()

	entries, err := c.List(ctx, "/Films")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if len(byName) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(byName), entries)
	}
	if e := byName["a.mkv"]; e.IsDir || e.Size != 2048 || e.Path != "/Films/a.mkv" {
		t.Errorf("a.mkv = %+v", e)
	}
	if e := byName["Sub"]; !e.IsDir || e.Path != "/Films/Sub" {
		t.Errorf("Sub = %+v", e)
	}

	size, _, err := c.Stat(ctx, "/Films/Sub/b.mp4")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != 4096 {
		t.Errorf("size = %d, want 4096", size)
	}
}

func TestBrowseConnectionIsReused(t *testing.T) {
	addr, root := startTestFTP(t)
	writeTestFile(t, root, "a.mkv", 1024)
	c := newTestClient(t, addr)
	ctx := context.Background()

	for range 5 {
		if _, err := c.List(ctx, "/"); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if c.browse == nil {
		t.Error("browse connection was not kept alive across listings")
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"":                  "/",
		"/":                 "/",
		"Films":             "/Films",
		"/Films/":           "/Films",
		"/Disque dur/Films": "/Disque dur/Films",
		"/a/../b":           "/b",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
