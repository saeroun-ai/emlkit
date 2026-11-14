package mail_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// Tests of the parse's spool budgets, Limits.MaxMemorySpoolBytes and
// MaxDiskSpoolBytes, applied by Spool.Body to the trees ReadTree reads from a
// parse (#43, #70; spool_limits.go).

// readSpooledUnder parses text under limits and spools its tree.
func readSpooledUnder(t *testing.T, text string, limits message.Limits, opts *mail.SpoolOptions) (*mail.Tree, *mail.Spool) {
	t.Helper()
	_, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(opts)
	t.Cleanup(func() { spool.Close() })
	tree, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
	if err != nil {
		t.Fatalf("ReadTree() = %v", err)
	}
	return tree, spool
}

type keptPart struct {
	size int64
	cut  bool
}

func keptParts(t *testing.T, spool *mail.Spool, tree *mail.Tree) []keptPart {
	t.Helper()
	var out []keptPart
	for _, p := range tree.Root.Children {
		size, cut, ok := spool.Kept(p)
		if !ok {
			t.Fatalf("no entry for %v", p.Path)
		}
		out = append(out, keptPart{size, cut})
	}
	return out
}

func equalKept(got []keptPart, want ...keptPart) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// readBack reads what the spool keeps of n and checks it is the first size
// bytes of a body of byte c.
func readBack(t *testing.T, spool *mail.Spool, n *mail.Node, c byte, size int) {
	t.Helper()
	rc, ok, err := spool.Open(n)
	if !ok || err != nil {
		t.Fatalf("%v: Open() = %v, %v", n.Path, ok, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{c}, size)) {
		t.Errorf("%v: read back %d bytes (err %v), want %d of %q", n.Path, len(b), err, size, c)
	}
}

func TestSpoolLimits_diskStricterInParse(t *testing.T) {
	dir := t.TempDir()
	tree, spool := readSpooledUnder(t, spoolMessage(300, 300, 300),
		message.Limits{MaxDiskSpoolBytes: 400},
		&mail.SpoolOptions{MemoryBytes: -1, Dir: dir}) // DiskBytes: the 1 GiB default
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{300, false}, keptPart{100, true}, keptPart{0, true}) {
		t.Errorf("kept %v, want 300 whole, 100 cut, 0 cut", k)
	}
	if _, disk := spool.Usage(); disk != 400 {
		t.Errorf("disk usage %d, want the parse's 400", disk)
	}
	readBack(t, spool, tree.Root.Children[1], 'b', 100)
	d := spool.Diagnostics()
	if len(d) != 2 {
		t.Fatalf("diagnostics %v, want two cuts", d)
	}
	for i, x := range d {
		if x.Code != message.CodeLimitDiskSpoolBytes || x.Path[0] != i+1 || !strings.Contains(x.Message, "MaxDiskSpoolBytes") {
			t.Errorf("diagnostic %d: %+v, want limit.disk_spool_bytes naming MaxDiskSpoolBytes at part %d", i, x, i+1)
		}
	}
	// The tree still knows each whole body.
	for _, p := range tree.Root.Children {
		if p.Size != 300 {
			t.Errorf("%v: tree size %d, want 300", p.Path, p.Size)
		}
	}
}

func TestSpoolLimits_diskStricterInSpool(t *testing.T) {
	tree, spool := readSpooledUnder(t, spoolMessage(300, 300, 300),
		message.Limits{MaxDiskSpoolBytes: 10000},
		&mail.SpoolOptions{MemoryBytes: -1, DiskBytes: 400, Dir: t.TempDir()})
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{300, false}, keptPart{100, true}, keptPart{0, true}) {
		t.Errorf("kept %v, want 300 whole, 100 cut, 0 cut", k)
	}
	for _, x := range spool.Diagnostics() {
		if strings.Contains(x.Message, "MaxDiskSpoolBytes") {
			t.Errorf("%q names the parse's budget; the spool's quota cut it", x.Message)
		}
	}
}

// A memory budget sends a body to disk whole: nothing is lost, nothing is
// reported, and the budget counts every body of the tree together.
func TestSpoolLimits_memoryStricterInParse(t *testing.T) {
	dir := t.TempDir()
	tree, spool := readSpooledUnder(t, spoolMessage(40, 40, 40, 0),
		message.Limits{MaxMemorySpoolBytes: 100},
		&mail.SpoolOptions{Dir: dir}) // MemoryBytes: the 8 MiB default
	if mem, disk := spool.Usage(); mem != 80 || disk != 40 {
		t.Errorf("usage memory %d disk %d, want 80 and 40", mem, disk)
	}
	if n := spoolFiles(t, dir); n != 1 {
		t.Errorf("%d temporary files, want the third body's", n)
	}
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{40, false}, keptPart{40, false}, keptPart{40, false}, keptPart{0, false}) {
		t.Errorf("kept %v, want every body whole", k)
	}
	for i, c := range []byte("abc") {
		readBack(t, spool, tree.Root.Children[i], c, 40)
	}
	if d := spool.Diagnostics(); len(d) != 0 {
		t.Errorf("a body spilled to disk was reported: %v", d)
	}
}

func TestSpoolLimits_memoryStricterInSpool(t *testing.T) {
	dir := t.TempDir()
	tree, spool := readSpooledUnder(t, spoolMessage(40, 40, 40),
		message.Limits{MaxMemorySpoolBytes: 1 << 20},
		&mail.SpoolOptions{MemoryBytes: 100, Dir: dir})
	if mem, disk := spool.Usage(); mem != 80 || disk != 40 {
		t.Errorf("usage memory %d disk %d, want 80 and 40", mem, disk)
	}
	readBack(t, spool, tree.Root.Children[2], 'c', 40)
}

// LimitDisabled bounds nothing: the spool's own quotas alone apply.
func TestSpoolLimits_disabled(t *testing.T) {
	disabled := message.Limits{MaxMemorySpoolBytes: message.LimitDisabled, MaxDiskSpoolBytes: message.LimitDisabled}
	tree, spool := readSpooledUnder(t, spoolMessage(300, 300, 300), disabled,
		&mail.SpoolOptions{MemoryBytes: 350, DiskBytes: 400, Dir: t.TempDir()})
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{300, false}, keptPart{300, false}, keptPart{100, true}) {
		t.Errorf("kept %v, want 300 in memory, 300 on disk, 100 cut", k)
	}
	if mem, disk := spool.Usage(); mem != 300 || disk != 400 {
		t.Errorf("usage memory %d disk %d, want 300 and 400", mem, disk)
	}
}

// A negative SpoolOptions quota keeps nothing there, whatever room the parse
// gives.
func TestSpoolLimits_negativeSpoolKeepsNothing(t *testing.T) {
	dir := t.TempDir()
	tree, spool := readSpooledUnder(t, spoolMessage(10, 0),
		message.Limits{MaxMemorySpoolBytes: 1 << 20, MaxDiskSpoolBytes: 1 << 20},
		&mail.SpoolOptions{MemoryBytes: -1, DiskBytes: -1, Dir: dir})
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{0, true}, keptPart{0, false}) {
		t.Errorf("kept %v, want nothing of the first body and the empty one", k)
	}
	if mem, disk := spool.Usage(); mem != 0 || disk != 0 || spoolFiles(t, dir) != 0 {
		t.Errorf("usage memory %d disk %d, %d files; want nothing", mem, disk, spoolFiles(t, dir))
	}
}

// A zero budget is the default, not a budget of nothing and not no budget:
// small bodies stay in memory, and a body past 8 MiB goes to disk although
// the spool would keep 16 MiB in memory.
func TestSpoolLimits_zeroIsDefault(t *testing.T) {
	const def = 8 << 20
	if l := message.DefaultLimits(); l.MaxMemorySpoolBytes != def {
		t.Fatalf("default MaxMemorySpoolBytes %d, want %d", l.MaxMemorySpoolBytes, def)
	}
	// def+1 bytes in lines of 64, well within MaxLineBytes.
	big := strings.Repeat(strings.Repeat("b", 62)+"\r\n", def/64) + "b"
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n\r\n" + strings.Repeat("a", 10) + "\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n\r\n" + big + "\r\n" +
		"--B--\r\n"
	dir := t.TempDir()
	tree, spool := readSpooledUnder(t, text, message.Limits{},
		&mail.SpoolOptions{MemoryBytes: 2 * def, Dir: dir})
	if mem, disk := spool.Usage(); mem != 10 || disk != def+1 {
		t.Errorf("usage memory %d disk %d, want 10 and %d", mem, disk, def+1)
	}
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{10, false}, keptPart{def + 1, false}) {
		t.Errorf("kept %v, want both whole", k)
	}
}

// Each tree has its own parse budget; the spool's quotas count both.
func TestSpoolLimits_perTree(t *testing.T) {
	spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: -1, DiskBytes: 500, Dir: t.TempDir()})
	defer spool.Close()
	var trees []*mail.Tree
	for range 2 {
		_, e, err := message.Parse(strings.NewReader(spoolMessage(200, 200)),
			&message.ParseOptions{Limits: message.Limits{MaxDiskSpoolBytes: 300}})
		if err != nil {
			t.Fatal(err)
		}
		tree, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
		if err != nil {
			t.Fatal(err)
		}
		trees = append(trees, tree)
	}
	// First tree: 200 and 100 under its budget of 300. Second: 200 under the
	// spool's 200 left, then nothing; its own budget would allow 100 more.
	if k := keptParts(t, spool, trees[0]); !equalKept(k, keptPart{200, false}, keptPart{100, true}) {
		t.Errorf("first tree kept %v", k)
	}
	if k := keptParts(t, spool, trees[1]); !equalKept(k, keptPart{200, false}, keptPart{0, true}) {
		t.Errorf("second tree kept %v", k)
	}
	if _, disk := spool.Usage(); disk != 500 {
		t.Errorf("disk usage %d, want 500", disk)
	}
}

// An embedded message read as part of the tree counts against the same budget.
func TestSpoolLimits_embeddedShareBudget(t *testing.T) {
	inner := "Content-Type: text/plain\r\n\r\n" + strings.Repeat("i", 60) + "\r\n"
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n\r\n" + strings.Repeat("o", 60) + "\r\n" +
		"--B\r\nContent-Type: message/rfc822\r\n\r\n" + inner +
		"--B--\r\n"
	_, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{Limits: message.Limits{MaxDiskSpoolBytes: 100}})
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: -1, Dir: t.TempDir()})
	defer spool.Close()
	tree, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body, DescendMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	carrier := tree.Root.Children[1]
	if len(carrier.Children) != 1 {
		t.Fatalf("the carrier was not descended into: %+v", carrier)
	}
	size, cut, ok := spool.Kept(carrier.Children[0])
	if !ok || size != 40 || !cut {
		t.Errorf("embedded body kept %d cut %v ok %v, want 40 of it cut", size, cut, ok)
	}
}

// Entities no parse session saw, and nodes built by hand, are spooled under
// the SpoolOptions alone.
func TestSpoolLimits_noSession(t *testing.T) {
	e, err := message.Read(strings.NewReader(spoolMessage(300, 300)))
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: -1, DiskBytes: 500, Dir: t.TempDir()})
	defer spool.Close()
	tree, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
	if err != nil {
		t.Fatal(err)
	}
	if k := keptParts(t, spool, tree); !equalKept(k, keptPart{300, false}, keptPart{200, true}) {
		t.Errorf("kept %v, want the spool's quota alone", k)
	}

	manual := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: 4, DiskBytes: 6, Dir: t.TempDir()})
	defer manual.Close()
	n := &mail.Node{Path: []int{0}}
	if err := manual.Body(n, strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	if size, cut, _ := manual.Kept(n); size != 6 || !cut {
		t.Errorf("hand-built node kept %d cut %v, want 6 cut", size, cut)
	}
}

// A store failure under a parse budget ends ReadTree, and Close deletes every
// blob made, the incomplete one too.
func TestSpoolLimits_storeFailureCleanup(t *testing.T) {
	store := &memStore{failAt: 3}
	_, e, err := message.Parse(strings.NewReader(spoolMessage(50, 60, 70)),
		&message.ParseOptions{Limits: message.Limits{MaxMemorySpoolBytes: 10, MaxDiskSpoolBytes: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(&mail.SpoolOptions{Store: store})
	if _, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body}); err == nil || !strings.Contains(err.Error(), "store is full") {
		t.Errorf("ReadTree() = %v, want the store's failure", err)
	}
	if _, disk := spool.Usage(); disk != 110 {
		t.Errorf("disk usage %d, want the two bodies stored", disk)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 2 || len(store.blobs) != 0 {
		t.Errorf("deleted %v, %d blobs left", store.deleted, len(store.blobs))
	}
}

// cancelAt is an input that cancels the parse's context once at bytes have
// been read from it, and then goes on delivering.
type cancelAt struct {
	r      io.Reader
	at     int
	n      int
	cancel context.CancelFunc
}

func (c *cancelAt) Read(p []byte) (int, error) {
	if len(p) > 64 {
		p = p[:64]
	}
	n, err := c.r.Read(p)
	c.n += n
	if c.n >= c.at && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return n, err
}

// A parse cancelled while a body is spooled ends ReadTree with the stop, and
// Close removes every temporary file.
func TestSpoolLimits_cancelCleanup(t *testing.T) {
	dir := t.TempDir()
	text := spoolMessage(500, 500, 500)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := &cancelAt{r: strings.NewReader(text), at: len(text) / 2, cancel: cancel}
	_, e, err := message.ParseContext(ctx, in, &message.ParseOptions{Limits: message.Limits{MaxMemorySpoolBytes: 100, MaxDiskSpoolBytes: 700}})
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(&mail.SpoolOptions{Dir: dir})
	_, err = mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
	if err == nil || e.ParseStop() == nil {
		t.Errorf("ReadTree() = %v with stop %v, want the cancellation", err, e.ParseStop())
	}
	if _, disk := spool.Usage(); disk > 700 {
		t.Errorf("disk usage %d past the parse's 700", disk)
	}
	if n := spoolFiles(t, dir); n == 0 {
		t.Error("nothing was spooled before the cancellation; the test does not reach a spooled body")
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if n := spoolFiles(t, dir); n != 0 {
		t.Errorf("%d temporary files left after Close", n)
	}
}

// The storage contract of Spool.Body: errors of the caller's writer are
// passed on without being looked into, a short or impossible write is an
// error and not a body with a hole, and the writer's Close decides whether a
// blob is kept, even of a body the quota cut.

// probeErr counts every call that would look into it.
type probeErr struct{ unwraps, ises, ases *int }

func (e probeErr) Error() string { return "store writer failed" }
func (e probeErr) Unwrap() error { *e.unwraps++; return nil }
func (e probeErr) Is(error) bool { *e.ises++; return false }
func (e probeErr) As(any) bool   { *e.ases++; return false }

// writerStore is a BlobStore whose writers are made by newW.
type writerStore struct {
	memStore
	newW func(*bytes.Buffer) io.WriteCloser
}

func (s *writerStore) Create() (io.WriteCloser, string, error) {
	w, key, err := s.memStore.Create()
	if err != nil {
		return nil, "", err
	}
	return s.newW(w.(memBlob).Buffer), key, nil
}

type funcWriter struct {
	buf   *bytes.Buffer
	write func(b *bytes.Buffer, p []byte) (int, error)
	close func() error
}

func (w funcWriter) Write(p []byte) (int, error) { return w.write(w.buf, p) }
func (w funcWriter) Close() error {
	if w.close != nil {
		return w.close()
	}
	return nil
}

func spoolInto(t *testing.T, store *writerStore, diskBytes int64) (*mail.Spool, error) {
	t.Helper()
	_, e, err := message.Parse(strings.NewReader(spoolMessage(100)), nil)
	if err != nil {
		t.Fatal(err)
	}
	spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: -1, DiskBytes: diskBytes, Store: store})
	_, err = mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
	return spool, err
}

func closeDeletesAll(t *testing.T, spool *mail.Spool, store *writerStore) {
	t.Helper()
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != store.n || len(store.blobs) != 0 {
		t.Errorf("deleted %v of %d blobs, %d left", store.deleted, store.n, len(store.blobs))
	}
}

func TestSpoolStorage_writerErrorNotInspected(t *testing.T) {
	var unwraps, ises, ases int
	werr := probeErr{&unwraps, &ises, &ases}
	store := &writerStore{newW: func(b *bytes.Buffer) io.WriteCloser {
		return funcWriter{buf: b, write: func(*bytes.Buffer, []byte) (int, error) { return 0, werr }}
	}}
	spool, err := spoolInto(t, store, 0)
	if err == nil || errors.Unwrap(err) != error(werr) {
		t.Errorf("ReadTree() = %v, want the writer's error wrapped once", err)
	}
	if unwraps+ises+ases != 0 {
		t.Errorf("the writer's error was looked into: Unwrap %d, Is %d, As %d", unwraps, ises, ases)
	}
	closeDeletesAll(t, spool, store)
}

func TestSpoolStorage_shortWrite(t *testing.T) {
	store := &writerStore{newW: func(b *bytes.Buffer) io.WriteCloser {
		return funcWriter{buf: b, write: func(b *bytes.Buffer, p []byte) (int, error) {
			return b.Write(p[:len(p)-1])
		}}
	}}
	spool, err := spoolInto(t, store, 0)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("ReadTree() = %v, want io.ErrShortWrite", err)
	}
	closeDeletesAll(t, spool, store)
}

func TestSpoolStorage_impossibleWriteCount(t *testing.T) {
	for _, delta := range []int{1, -1000} {
		store := &writerStore{newW: func(b *bytes.Buffer) io.WriteCloser {
			return funcWriter{buf: b, write: func(b *bytes.Buffer, p []byte) (int, error) {
				b.Write(p)
				return max(len(p)+delta, -1), nil
			}}
		}}
		spool, err := spoolInto(t, store, 0)
		if err == nil || !strings.Contains(err.Error(), "invalid write result") {
			t.Errorf("count %+d: ReadTree() = %v, want an invalid write result", delta, err)
		}
		if mem, disk := spool.Usage(); mem != 0 || disk != 0 {
			t.Errorf("count %+d: the failed body is counted: %d, %d", delta, mem, disk)
		}
		closeDeletesAll(t, spool, store)
	}
}

// A Close that fails is a storage failure even when the quota cut the body.
func TestSpoolStorage_closeFailsAfterCut(t *testing.T) {
	cerr := errors.New("flush failed")
	store := &writerStore{newW: func(b *bytes.Buffer) io.WriteCloser {
		return funcWriter{buf: b, write: func(b *bytes.Buffer, p []byte) (int, error) { return b.Write(p) },
			close: func() error { return cerr }}
	}}
	spool, err := spoolInto(t, store, 40)
	if errors.Unwrap(err) != cerr {
		t.Errorf("ReadTree() = %v, want the Close failure", err)
	}
	if d := spool.Diagnostics(); len(d) != 0 {
		t.Errorf("a body that was not kept is reported cut: %v", d)
	}
	closeDeletesAll(t, spool, store)
}
