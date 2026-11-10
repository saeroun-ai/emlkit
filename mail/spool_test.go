package mail_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

func spoolMessage(sizes ...int) string {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i, n := range sizes {
		b.WriteString("--B\r\nContent-Type: application/octet-stream\r\n\r\n")
		b.WriteString(strings.Repeat(string(rune('a'+i)), n))
		b.WriteString("\r\n")
	}
	b.WriteString("--B--\r\n")
	return b.String()
}

func readSpooled(t *testing.T, text string, opts *mail.SpoolOptions) (*mail.Tree, *mail.Spool) {
	t.Helper()
	_, e, err := message.Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(opts)
	tree, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
	if err != nil {
		spool.Close()
		t.Fatalf("ReadTree() = %v", err)
	}
	return tree, spool
}

func spoolFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestSpool_memoryThenDisk(t *testing.T) {
	dir := t.TempDir()
	tree, spool := readSpooled(t, spoolMessage(10, 100, 0, 5000), &mail.SpoolOptions{MemoryBytes: 64, Dir: dir})
	parts := tree.Root.Children
	if mem, disk := spool.Usage(); mem != 10 || disk != 5100 {
		t.Errorf("usage memory %d disk %d, want 10 and 5100", mem, disk)
	}
	if n := spoolFiles(t, dir); n != 2 {
		t.Errorf("%d temporary files, want 2", n)
	}
	// Every body reads back whole, any number of times, independently.
	for i, want := range []int{10, 100, 0, 5000} {
		for round := 0; round < 2; round++ {
			rc, ok, err := spool.Open(parts[i])
			if !ok || err != nil {
				t.Fatalf("part %d: Open() = %v, %v", i, ok, err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if len(b) != want || (want > 0 && b[0] != byte('a'+i)) {
				t.Errorf("part %d round %d: read %d bytes, want %d", i, round, len(b), want)
			}
		}
		if parts[i].Size != int64(want) || len(parts[i].Data) != 0 {
			t.Errorf("part %d: tree size %d, data %d bytes; want the size and nothing kept by the tree", i, parts[i].Size, len(parts[i].Data))
		}
	}
	if _, ok, _ := spool.Open(tree.Root); ok {
		t.Error("Open of a container reported a body")
	}
	// Close removes everything, and may be called again.
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if n := spoolFiles(t, dir); n != 0 {
		t.Errorf("%d temporary files left after Close", n)
	}
	if _, ok, _ := spool.Open(parts[0]); ok {
		t.Error("Open after Close reported a body")
	}
	if err := spool.Body(parts[0], strings.NewReader("x")); err == nil {
		t.Error("Body after Close succeeded")
	}
}

func TestSpool_diskQuota(t *testing.T) {
	dir := t.TempDir()
	tree, spool := readSpooled(t, spoolMessage(300, 300, 300), &mail.SpoolOptions{MemoryBytes: -1, DiskBytes: 400, Dir: dir})
	defer spool.Close()
	var kept []int64
	var cuts []bool
	for _, p := range tree.Root.Children {
		size, cut, ok := spool.Kept(p)
		if !ok {
			t.Fatal("no entry")
		}
		kept, cuts = append(kept, size), append(cuts, cut)
		// The tree still knows the whole body.
		if p.Size != 300 {
			t.Errorf("tree size %d, want 300", p.Size)
		}
	}
	if kept[0] != 300 || kept[1] != 100 || kept[2] != 0 || cuts[0] || !cuts[1] || !cuts[2] {
		t.Errorf("kept %v cut %v, want [300 100 0] and [false true true]", kept, cuts)
	}
	if _, disk := spool.Usage(); disk != 400 {
		t.Errorf("disk usage %d, want the quota", disk)
	}
	d := spool.Diagnostics()
	if len(d) != 2 || d[0].Code != message.CodeLimitDiskSpoolBytes || d[0].Path[0] != 1 || d[1].Path[0] != 2 {
		t.Errorf("diagnostics %v", d)
	}
}

// memStore is a BlobStore in memory that counts what it is asked.
type memStore struct {
	blobs   map[string]*bytes.Buffer
	deleted []string
	failAt  int
	n       int
}

type memBlob struct {
	*bytes.Buffer
}

func (memBlob) Close() error { return nil }

func (m *memStore) Create() (io.WriteCloser, string, error) {
	m.n++
	if m.failAt == m.n {
		return nil, "", errors.New("store is full")
	}
	key := string(rune('A' + m.n))
	if m.blobs == nil {
		m.blobs = make(map[string]*bytes.Buffer)
	}
	m.blobs[key] = &bytes.Buffer{}
	return memBlob{m.blobs[key]}, key, nil
}

func (m *memStore) Open(key string) (io.ReadCloser, error) {
	b, ok := m.blobs[key]
	if !ok {
		return nil, errors.New("no such blob")
	}
	return io.NopCloser(bytes.NewReader(b.Bytes())), nil
}

func (m *memStore) Delete(key string) error {
	m.deleted = append(m.deleted, key)
	delete(m.blobs, key)
	return nil
}

func TestSpool_blobStore(t *testing.T) {
	store := &memStore{}
	tree, spool := readSpooled(t, spoolMessage(50, 60), &mail.SpoolOptions{MemoryBytes: -1, Store: store})
	rc, ok, err := spool.Open(tree.Root.Children[1])
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	b, _ := io.ReadAll(rc)
	if len(b) != 60 || len(store.blobs) != 2 {
		t.Errorf("read %d bytes from the store, %d blobs", len(b), len(store.blobs))
	}
	spool.Close()
	if len(store.deleted) != 2 || len(store.blobs) != 0 {
		t.Errorf("deleted %v, %d blobs left", store.deleted, len(store.blobs))
	}

	// A store that fails ends ReadTree, and Close still deletes what was made.
	store = &memStore{failAt: 2}
	_, e, _ := message.Parse(strings.NewReader(spoolMessage(50, 60, 70)), nil)
	spool = mail.NewSpool(&mail.SpoolOptions{MemoryBytes: -1, Store: store})
	if _, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body}); err == nil || !strings.Contains(err.Error(), "store is full") {
		t.Errorf("ReadTree() = %v, want the store's failure", err)
	}
	spool.Close()
	if len(store.deleted) != 1 || len(store.blobs) != 0 {
		t.Errorf("after a failed store: deleted %v, %d blobs left", store.deleted, len(store.blobs))
	}
}
