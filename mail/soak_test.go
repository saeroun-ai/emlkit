package mail_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/charset"
	"github.com/saeroun-ai/emlkit/mail"
)

// Importing the charset plugin sets message.CharsetReader for the whole test
// binary, and the other tests of this package expect none: an unknown charset
// they rely on would be decoded. The default is put back here, and TestSoak
// installs the plugin for its own run.
func init() { message.CharsetReader = nil }

// The randomized soak (ROB-130-007). Every iteration generates one input from
// the seed and its own number — a mutated corpus fixture or a generated
// message damaged afterwards — parses it whole, in small chunks and sometimes
// through a hostile reader, consumes it one of five ways, and checks what the
// parse reports. Every few iterations it checks that no goroutine or file
// descriptor was left behind and that the heap after a collection stays under
// a ceiling and does not grow. docs/robustness-notes/soak.md has the account.
//
// Without configuration it runs a short fixed pass. EMLKIT_SOAK=30s (a
// duration) or EMLKIT_SOAK=5000 (a count) makes it long, EMLKIT_SOAK_SEED
// picks the seed, EMLKIT_SOAK_ITER=12,34 runs those iterations alone,
// EMLKIT_SOAK_KEEP_GOING=1 reports each failure in a line and goes on, and
// EMLKIT_SOAK_DUMP=<dir> keeps the input of a failure. Each iteration depends
// on the seed and its number only, so the command a failure prints
// reproduces it exactly. EMLKIT_SOAK_OPEN=1 runs the tests that pin open
// findings, at the end of the file.

const (
	skMaxInput     = 64 << 10         // every generated input is cut here
	skDefaultSeed  = 1300007          // ROB-130-007
	skShortIters   = 3000             // the pass every go test runs
	skHeapCeiling  = 64 << 20         // live heap after a collection
	skAllocCeiling = 64 << 20         // bytes one iteration may allocate; 8 MiB is seen, 9 under -race
	skIterDeadline = 30 * time.Second // the watchdog; an iteration takes milliseconds
	skMaxCalls     = 1 << 16          // callbacks or NextPart calls one consumer makes
)

// skControl is how the run was asked for.
type skControl struct {
	seed     uint64
	iters    uint64        // run this many iterations, or
	duration time.Duration // run for this long, or
	only     []uint64      // run these iterations alone, or
	every    uint64        // iterations between leak and heap checks

	// keepGoing reports a failed iteration in one line and goes on, for
	// surveying a seed; EMLKIT_SOAK_KEEP_GOING=1.
	keepGoing bool
}

func skControls(t *testing.T) skControl {
	c := skControl{seed: skDefaultSeed, iters: skShortIters, every: 100}
	if v := os.Getenv("EMLKIT_SOAK_SEED"); v != "" {
		s, err := strconv.ParseUint(v, 0, 64)
		if err != nil {
			t.Fatalf("EMLKIT_SOAK_SEED=%q: %v", v, err)
		}
		c.seed = s
	}
	c.keepGoing = os.Getenv("EMLKIT_SOAK_KEEP_GOING") != ""
	if v := os.Getenv("EMLKIT_SOAK_ITER"); v != "" {
		for f := range strings.SplitSeq(v, ",") {
			n, err := strconv.ParseUint(strings.TrimSpace(f), 10, 62)
			if err != nil {
				t.Fatalf("EMLKIT_SOAK_ITER=%q: %v", v, err)
			}
			c.only = append(c.only, n)
		}
		c.iters = 0
		return c
	}
	if v := os.Getenv("EMLKIT_SOAK"); v != "" {
		if testing.Short() {
			t.Skip("EMLKIT_SOAK asks for the long soak, which -short skips")
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.iters, c.duration, c.every = 0, d, 1000
		} else if n, err := strconv.ParseUint(v, 10, 62); err == nil && n > 0 {
			c.iters, c.every = n, 1000
		} else {
			t.Fatalf("EMLKIT_SOAK=%q is neither a duration nor an iteration count", v)
		}
	}
	return c
}

// iteration returns the number of the k-th iteration to run, and false once
// the run is over.
func (c skControl) iteration(k uint64, start time.Time) (uint64, bool) {
	switch {
	case len(c.only) > 0:
		if k < uint64(len(c.only)) {
			return c.only[k], true
		}
		return 0, false
	case c.duration > 0:
		return k, time.Since(start) < c.duration
	}
	return k, k < c.iters
}

// reproduceIter is the command that runs one iteration again.
func (c skControl) reproduceIter(iter uint64) string {
	return fmt.Sprintf("env EMLKIT_SOAK_SEED=%d EMLKIT_SOAK_ITER=%d go test -run '^TestSoak$' -v ./mail/", c.seed, iter)
}

// reproduceRun is the command that runs the first n iterations again, for a
// failure that belongs to the run rather than to one iteration.
func (c skControl) reproduceRun(n uint64) string {
	return fmt.Sprintf("env EMLKIT_SOAK_SEED=%d EMLKIT_SOAK=%d go test -run '^TestSoak$' -v ./mail/", c.seed, n)
}

func TestSoak(t *testing.T) {
	ctl := skControls(t)
	old := message.CharsetReader
	message.CharsetReader = charset.Reader
	t.Cleanup(func() { message.CharsetReader = old })
	h := &skHarness{ctl: ctl, fixtures: skLoadFixtures(t), spoolDir: t.TempDir()}

	baseG := runtime.NumGoroutine()
	baseFD := skOpenFDs()
	var heap []uint64
	var ran uint64
	checkpoint := func() {
		t.Helper()
		if n := skSettle(baseG); n > baseG {
			t.Fatalf("%d goroutines after %d iterations, %d before the first: a parse left one running\n  reproduce: %s", n, ran, baseG, ctl.reproduceRun(ran))
		}
		if baseFD >= 0 {
			if n := skOpenFDs(); n > baseFD+2 {
				t.Fatalf("%d open file descriptors after %d iterations, %d before the first\n  reproduce: %s", n, ran, baseFD, ctl.reproduceRun(ran))
			}
		}
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heap = append(heap, ms.HeapAlloc)
		if ms.HeapAlloc > skHeapCeiling {
			t.Fatalf("live heap %d bytes after %d iterations, past the ceiling of %d\n  reproduce: %s", ms.HeapAlloc, ran, skHeapCeiling, ctl.reproduceRun(ran))
		}
	}
	checkpoint()

	var (
		inputBytes int64
		maxAlloc   uint64
		modes      [skModes]int
		states     = map[string]int{}
		codes      = map[message.Code]int{}
	)
	start := time.Now()
	for k := uint64(0); ; k++ {
		iter, ok := ctl.iteration(k, start)
		if !ok {
			break
		}
		res := h.runOne(iter)
		ran++
		switch {
		case res.hung:
			h.fail(t, iter, fmt.Sprintf("did not finish within %v\n%s", skIterDeadline, skCut(res.stack, 64<<10)))
		case res.panicVal != nil:
			h.fail(t, iter, fmt.Sprintf("panicked: %v\n%s", res.panicVal, res.stack))
		case res.err != nil:
			h.fail(t, iter, res.err.Error())
		}
		if res.stats.alloc > skAllocCeiling {
			h.fail(t, iter, fmt.Sprintf("allocated %d bytes for an input of %d", res.stats.alloc, res.stats.inBytes))
		}
		inputBytes += int64(res.stats.inBytes)
		maxAlloc = max(maxAlloc, res.stats.alloc)
		modes[res.stats.mode]++
		states[res.stats.state.String()]++
		for _, c := range res.stats.codes {
			codes[c]++
		}
		if len(ctl.only) > 0 {
			t.Logf("iteration %d: %d bytes, %s\n%s", iter, res.stats.inBytes, res.stats.desc, res.stats.outcome)
		}
		if ran%ctl.every == 0 {
			checkpoint()
		}
	}
	elapsed := time.Since(start)
	checkpoint()

	// The heap must not trend upward: the median of the last quarter of the
	// samples is held to that of the first, with a margin for noise.
	if q := len(heap) / 4; q >= 2 {
		first, last := skMedian(heap[1:1+q]), skMedian(heap[len(heap)-q:])
		if last > first+first/2+4<<20 {
			t.Fatalf("live heap grew from a median of %d to %d bytes over %d iterations\n  reproduce: %s", first, last, ran, ctl.reproduceRun(ran))
		}
	}
	names := make([]string, 0, len(codes))
	for c := range codes {
		names = append(names, string(c))
	}
	slices.Sort(names)
	t.Logf("soak: seed %d, %d iterations in %v (%.0f/s), %d input bytes; modes drain/walk/tree/spool/reader %v; final states %v; max allocation of one iteration %d bytes; live heap samples %d..%d bytes; %d distinct diagnostic codes",
		ctl.seed, ran, elapsed.Round(time.Millisecond), float64(ran)/max(elapsed.Seconds(), 1e-9), inputBytes, modes, states, maxAlloc, slices.Min(heap), slices.Max(heap), len(codes))
	if testing.Verbose() {
		t.Logf("codes seen: %s", strings.Join(names, " "))
	}
}

// skHarness runs iterations.
type skHarness struct {
	ctl      skControl
	fixtures [][]byte
	spoolDir string
}

type skStats struct {
	mode    int
	state   message.CompletionState
	codes   []message.Code
	inBytes int
	alloc   uint64
	desc    string
	outcome string
}

type skResult struct {
	stats    skStats
	err      error
	panicVal any
	stack    []byte
	hung     bool
}

// runOne runs one iteration under the watchdog and catches its panic.
func (h *skHarness) runOne(iter uint64) skResult {
	done := make(chan skResult, 1)
	go func() {
		var res skResult
		defer func() {
			if v := recover(); v != nil {
				res.panicVal, res.stack = v, debug.Stack()
			}
			done <- res
		}()
		before := skAllocated()
		res.stats, res.err = h.iteration(iter)
		res.stats.alloc = skAllocated() - before
	}()
	timer := time.NewTimer(skIterDeadline)
	defer timer.Stop()
	select {
	case res := <-done:
		return res
	case <-timer.C:
		buf := make([]byte, 1<<20)
		return skResult{hung: true, stack: buf[:runtime.Stack(buf, true)]}
	}
}

// fail reports a failed iteration with its input and how to run it again.
// The input is generated anew from the seed: it depends on nothing else.
func (h *skHarness) fail(t *testing.T, iter uint64, what string) {
	t.Helper()
	var (
		in  []byte
		cfg skConfig
	)
	if v := func() (v any) {
		defer func() { v = recover() }()
		var r *rand.Rand
		in, r = h.input(iter)
		cfg = skChoose(r, len(in))
		return nil
	}(); v != nil {
		t.Fatalf("soak seed %d, iteration %d: the soak's own generator panicked: %v\n%s", h.ctl.seed, iter, v, what)
	}
	dir := os.Getenv("EMLKIT_SOAK_DUMP")
	if dir == "" {
		dir = t.TempDir()
	}
	name := filepath.Join(dir, fmt.Sprintf("soak-%d-%d.eml", h.ctl.seed, iter))
	if err := os.WriteFile(name, in, 0o644); err != nil {
		name = "(not written: " + err.Error() + ")"
	}
	if h.ctl.keepGoing && len(h.ctl.only) == 0 {
		first, _, _ := strings.Cut(what, "\n")
		t.Errorf("iteration %d (%s, %s): %s", iter, skModeNames[cfg.mode], cfg.opts.Profile, first)
		return
	}
	report := t.Fatalf
	if h.ctl.keepGoing {
		report = t.Errorf
	}
	report("soak seed %d, iteration %d %s\n  config: %s\n  input: %d bytes, written to %s (set EMLKIT_SOAK_DUMP=<dir> to keep it)\n  reproduce: %s\n  quoted: %q\n%s",
		h.ctl.seed, iter, skCut([]byte(what), 16<<10), cfg.String(), len(in), name, h.ctl.reproduceIter(iter), skCut(in, 1<<10), hex.Dump(skCut(in, 256)))
}

func (h *skHarness) input(iter uint64) ([]byte, *rand.Rand) {
	r := rand.New(rand.NewPCG(h.ctl.seed, iter))
	g := &skGen{r: r, fixtures: h.fixtures}
	return g.input(), r
}

// iteration generates input iter, parses it whole and in small chunks — and,
// when the configuration says so, through a hostile reader — and checks each.
func (h *skHarness) iteration(iter uint64) (skStats, error) {
	in, r := h.input(iter)
	cfg := skChoose(r, len(in))
	st := skStats{mode: cfg.mode, inBytes: len(in), desc: cfg.String()}

	whole := &skPass{cfg: &cfg, in: in, src: bytes.NewReader(in), spoolDir: h.spoolDir}
	if err := whole.run(); err != nil {
		return st, fmt.Errorf("reading the input whole: %w", err)
	}
	st.state, st.codes, st.outcome = whole.state, whole.codes, whole.out.String()

	chunked := &skPass{cfg: &cfg, in: in, src: cfg.reader(in, nil), spoolDir: h.spoolDir}
	if err := chunked.run(); err != nil {
		return st, fmt.Errorf("reading the input in small chunks: %w", err)
	}
	// A budget is spent as bytes are read, and readers fetch ahead, so only
	// a parse under the default budgets must not depend on the chunking. A
	// sink that stops the parse at its n-th diagnostic is compared too: the
	// diagnostics come in one order, so the stop falls in one place.
	// ProfileStrict keeps a base64 decoder whose verdict depends on where
	// reads end, which is its earlier behavior (see
	// TestSoak_strictBase64Chunked and docs/robustness-notes/soak.md).
	comparable := !cfg.small && cfg.opts.Profile != message.ProfileStrict
	if comparable && whole.out.String() != chunked.out.String() {
		return st, fmt.Errorf("reading the input in chunks of at most %d bytes changed the outcome\nwhole:\n%s\nchunked:\n%s", cfg.chunkMax, whole.out.String(), chunked.out.String())
	}

	if cfg.hostile != skHostileNone {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hostile := &skPass{cfg: &cfg, in: in, ctx: ctx, src: cfg.reader(in, cancel), spoolDir: h.spoolDir, hostile: true}
		if err := hostile.run(); err != nil {
			return st, fmt.Errorf("reading the input through a %s reader: %w", skHostileNames[cfg.hostile], err)
		}
	}
	return st, nil
}

func skLoadFixtures(t *testing.T) [][]byte {
	files, err := filepath.Glob(filepath.Join("..", "testdata", "corpus", "*.eml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus fixtures: %v", err)
	}
	slices.Sort(files)
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// skSettle waits a little for goroutines that are ending, and returns how
// many are left.
func skSettle(base int) int {
	n := runtime.NumGoroutine()
	for i := 0; i < 200 && n > base; i++ {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// skOpenFDs counts the process's open file descriptors where /proc says, and
// is -1 elsewhere.
func skOpenFDs() int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

func skAllocated() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

func skMedian(xs []uint64) uint64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

func skCut(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// ---------------------------------------------------------------------------
// Configuration

const (
	skModeDrain = iota
	skModeWalk
	skModeTree
	skModeSpool
	skModeReader
	skModes
)

var skModeNames = [skModes]string{"drain", "walk", "tree", "spool", "reader"}

const (
	skHostileNone = iota
	skHostileIOError
	skHostileCancel
	skHostileStall
	skHostiles
)

var skHostileNames = [skHostiles]string{"plain", "failing", "cancelling", "stalling"}

var (
	skErrIO   = errors.New("soak: the input failed")
	skErrStop = errors.New("soak: the sink stops the parse")
)

// skConfig is how one iteration parses and consumes its input; it is chosen
// from the iteration's generator, so it is as repeatable as the input.
type skConfig struct {
	opts  message.ParseOptions
	small bool // some budget is small

	mode    int
	partial int64 // bytes of the root body read before Drain
	tree    mail.TreeOptions

	spoolMem, spoolDisk int64
	store               bool
	storeFail           int // the Create call of the store that fails, or 0

	sink     bool
	sinkStop int // the sink stops the parse at this call, or never at 0
	discard  bool

	chunkMax  int
	chunkSeed [2]uint64
	hostile   int
	hostileAt int
}

func skChoose(r *rand.Rand, n int) skConfig {
	var c skConfig
	c.opts.Profile = []message.Profile{message.ProfileTolerant, message.ProfileForensic, message.ProfileStrict}[r.IntN(3)]
	if r.IntN(4) == 0 {
		c.opts.Compatibility = []message.Compatibility{message.CompatibilityAngus, message.CompatibilityMimeKit, message.CompatibilityPythonEmail}[r.IntN(3)]
	}
	c.opts.WalkEmbeddedMessages = r.IntN(2) == 0
	c.opts.RecognizeMboxEnvelope = r.IntN(4) == 0
	if r.IntN(6) == 0 {
		c.opts.FallbackCharset = skPick(r, "euc-kr", "windows-1252", "iso-2022-jp", "x-made-up")
	}
	c.opts.ReadOptions.SkipTextAttachmentDecoding = r.IntN(10) == 0
	if r.IntN(3) == 0 {
		c.small = true
		c.opts.Limits = skSmallLimits(r, n)
	}

	c.mode = r.IntN(skModes)
	switch c.mode {
	case skModeDrain:
		if r.IntN(2) == 0 {
			c.partial = 1 + r.Int64N(64)
		}
	case skModeTree, skModeSpool:
		c.tree.MaxBodyBytes = []int64{0, 1 + r.Int64N(512), -1}[r.IntN(3)]
		c.tree.MaxTotalBytes = []int64{0, 1 + r.Int64N(2048), -1}[r.IntN(3)]
		c.tree.DescendMessages = r.IntN(2) == 0
		if c.mode == skModeSpool {
			c.spoolMem = []int64{-1, 1 + r.Int64N(256), 1 + r.Int64N(4096), 0}[r.IntN(4)]
			c.spoolDisk = []int64{-1, 1 + r.Int64N(1024), 1 + r.Int64N(16384), 0}[r.IntN(4)]
			c.store = r.IntN(4) == 0
			if c.store && r.IntN(4) == 0 {
				c.storeFail = 1 + r.IntN(3)
			}
		}
	}

	if r.IntN(3) == 0 {
		c.sink = true
		if r.IntN(3) == 0 {
			c.sinkStop = 1 + r.IntN(6)
		}
		c.discard = r.IntN(3) == 0
	}

	c.chunkMax = []int{1, 3, 16, 64, 512, 4096}[r.IntN(6)]
	c.chunkSeed = [2]uint64{r.Uint64(), r.Uint64()}
	c.hostile = r.IntN(skHostiles)
	c.hostileAt = r.IntN(n + 1)
	return c
}

// skSmallLimits sets a few budgets to small values, and sometimes disables
// one.
func skSmallLimits(r *rand.Rand, n int) message.Limits {
	var l message.Limits
	fields := []struct {
		p   *int64
		max int64
	}{
		{&l.MaxInputBytes, int64(n) + 64},
		{&l.MaxHeaderBytes, 2048},
		{&l.MaxHeaderFieldBytes, 512},
		{&l.MaxHeaderCount, 16},
		{&l.MaxLineBytes, 1000},
		{&l.MaxTotalHeaderBytes, 4096},
		{&l.MaxTotalHeaderCount, 40},
		{&l.MaxParameterCount, 4},
		{&l.MaxParameterSegments, 4},
		{&l.MaxMIMEDepth, 4},
		{&l.MaxMessageDepth, 3},
		{&l.MaxPartCount, 8},
		{&l.MaxPreambleBytes, 256},
		{&l.MaxEpilogueBytes, 256},
		{&l.MaxEncodedBodyBytes, 4096},
		{&l.MaxDecodedBodyBytes, 4096},
		{&l.MaxTotalDecodedBytes, 8192},
		{&l.MaxDiagnosticCount, 8},
	}
	for range 1 + r.IntN(4) {
		f := fields[r.IntN(len(fields))]
		*f.p = 1 + r.Int64N(f.max)
	}
	if r.IntN(5) == 0 {
		*fields[r.IntN(len(fields))].p = message.LimitDisabled
	}
	return l
}

func (c *skConfig) String() string {
	limits := "default"
	if c.small {
		limits = fmt.Sprintf("%+v", c.opts.Limits)
	}
	return fmt.Sprintf("profile=%v compat=%v mode=%s walkEmbedded=%v mbox=%v fallback=%q skipText=%v limits=%s partial=%d tree=%d/%d/%v spool=%d/%d store=%v/%d sink=%v/%d/%v chunk=%d hostile=%s@%d",
		c.opts.Profile, c.opts.Compatibility, skModeNames[c.mode], c.opts.WalkEmbeddedMessages, c.opts.RecognizeMboxEnvelope,
		c.opts.FallbackCharset, c.opts.ReadOptions.SkipTextAttachmentDecoding, limits, c.partial,
		c.tree.MaxBodyBytes, c.tree.MaxTotalBytes, c.tree.DescendMessages, c.spoolMem, c.spoolDisk, c.store, c.storeFail,
		c.sink, c.sinkStop, c.discard, c.chunkMax, skHostileNames[c.hostile], c.hostileAt)
}

// reader returns the input as the small-chunk reader delivers it: at most
// chunkMax bytes a read, with an occasional short run of empty reads. Given
// cancel, it is the hostile reader the configuration names.
func (c *skConfig) reader(in []byte, cancel context.CancelFunc) io.Reader {
	cr := &skChunkReader{data: in, r: rand.New(rand.NewPCG(c.chunkSeed[0], c.chunkSeed[1])), max: c.chunkMax, at: -1}
	if cancel != nil {
		cr.kind, cr.at, cr.cancel = c.hostile, c.hostileAt, cancel
	}
	return cr
}

type skChunkReader struct {
	data   []byte
	off    int
	r      *rand.Rand
	max    int
	zeros  int
	kind   int
	at     int
	cancel context.CancelFunc
}

func (c *skChunkReader) Read(p []byte) (int, error) {
	if c.at >= 0 && c.off >= c.at {
		switch c.kind {
		case skHostileIOError:
			return 0, skErrIO
		case skHostileStall:
			return 0, nil
		case skHostileCancel:
			if c.cancel != nil {
				c.cancel()
				c.cancel = nil
			}
		}
	}
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if c.zeros > 0 {
		c.zeros--
		return 0, nil
	}
	if c.r.IntN(64) == 0 {
		c.zeros = c.r.IntN(5)
		return 0, nil
	}
	n := min(1+c.r.IntN(c.max), len(p), len(c.data)-c.off)
	if c.at > c.off && c.kind != skHostileCancel {
		n = min(n, c.at-c.off)
	}
	copy(p, c.data[c.off:c.off+n])
	c.off += n
	return n, nil
}

// skSink records what it is told and stops the parse when asked to.
type skSink struct {
	calls     int
	stopAfter int
	stopped   bool
	seen      []string
}

func (k *skSink) diagnose(_ *message.ParseSession, d message.Diagnostic) error {
	k.calls++
	if len(k.seen) < 4096 {
		k.seen = append(k.seen, fmt.Sprintf("%s %s %v", d.Severity, d.Code, d.Path))
	}
	if k.stopAfter > 0 && k.calls >= k.stopAfter {
		k.stopped = true
		return skErrStop
	}
	return nil
}

// skStore is an in-memory BlobStore that remembers what it was asked to
// delete.
type skStore struct {
	mu      sync.Mutex
	creates int
	failAt  int
	blobs   map[string]*bytes.Buffer
	deleted map[string]int
	keys    []string
}

func skNewStore(failAt int) *skStore {
	return &skStore{failAt: failAt, blobs: map[string]*bytes.Buffer{}, deleted: map[string]int{}}
}

func (s *skStore) Create() (io.WriteCloser, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if s.creates == s.failAt {
		return nil, "", errors.New("soak: the store is full")
	}
	key := "blob-" + strconv.Itoa(s.creates)
	b := &bytes.Buffer{}
	s.blobs[key] = b
	s.keys = append(s.keys, key)
	return &skBlobWriter{b: b}, key, nil
}

func (s *skStore) Open(key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[key]
	if !ok {
		return nil, fmt.Errorf("soak: no blob %q", key)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(b.Bytes()))), nil
}

func (s *skStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[key]++
	delete(s.blobs, key)
	return nil
}

// check reports a blob that was not deleted exactly once.
func (s *skStore) check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blobs) != 0 {
		return fmt.Errorf("the spool's store still holds %d blobs after Close", len(s.blobs))
	}
	for _, k := range s.keys {
		if s.deleted[k] != 1 {
			return fmt.Errorf("blob %s was deleted %d times", k, s.deleted[k])
		}
	}
	return nil
}

type skBlobWriter struct{ b *bytes.Buffer }

func (w *skBlobWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w *skBlobWriter) Close() error                { return nil }

// ---------------------------------------------------------------------------
// One pass: a parse, a consumer, and the checks.

var skCodeFormat = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

type skPass struct {
	cfg      *skConfig
	in       []byte
	src      io.Reader
	ctx      context.Context
	spoolDir string
	hostile  bool

	out      strings.Builder
	decoded  int64
	complete bool // the consumer read everything and ended at the end
	state    message.CompletionState
	codes    []message.Code
}

// bound is how many decoded bytes the input may yield in all: no decoder
// expands, uudecoding yields at most one and a half bytes for one, and a
// charset turns one byte into at most three.
func (p *skPass) bound() int64 { return 8*int64(len(p.in)) + 4096 }

// body reads one body to its end, counting it against the bound.
func (p *skPass) body(r io.Reader) (int64, [sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	if r == nil {
		return 0, sum, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, p.bound()-p.decoded+1))
	p.decoded += n
	copy(sum[:], h.Sum(nil))
	if p.decoded > p.bound() {
		return n, sum, fmt.Errorf("the bodies yielded more than %d bytes from %d bytes of input", p.bound(), len(p.in))
	}
	return n, sum, err
}

func (p *skPass) run() error {
	opts := p.cfg.opts
	var sink *skSink
	if p.cfg.sink {
		sink = &skSink{stopAfter: p.cfg.sinkStop}
		opts.DiagnosticSink = message.DiagnosticSinkFunc(sink.diagnose)
		opts.DiscardDeliveredDiagnostics = p.cfg.discard
	}
	var (
		s   *message.ParseSession
		e   *message.Entity
		err error
	)
	if p.ctx != nil {
		s, e, err = message.ParseContext(p.ctx, p.src, &opts)
	} else {
		s, e, err = message.Parse(p.src, &opts)
	}
	if s == nil {
		return errors.New("Parse returned no session")
	}
	if errors.Is(err, message.ErrInvalidLimits) {
		return fmt.Errorf("the soak's own budgets were refused: %v", err)
	}
	if e == nil {
		if err == nil {
			return errors.New("Parse returned no entity and no error")
		}
		fmt.Fprintf(&p.out, "no entity: fatal=%v\n", message.IsFatal(err))
	} else {
		var cerr error
		switch p.cfg.mode {
		case skModeDrain:
			cerr = p.drain(e)
		case skModeWalk:
			cerr = p.walk(e)
		case skModeTree, skModeSpool:
			cerr = p.tree(e)
		case skModeReader:
			cerr = p.reader(e)
		}
		if cerr != nil {
			return cerr
		}
	}
	return p.check(s, sink)
}

func (p *skPass) drain(e *message.Entity) error {
	if p.cfg.partial > 0 && e.MultipartReader() == nil {
		n, _, err := p.body(io.LimitReader(e.Body, p.cfg.partial))
		if p.decoded > p.bound() {
			return err
		}
		fmt.Fprintf(&p.out, "read %d first\n", n)
	}
	err := e.Drain()
	fmt.Fprintf(&p.out, "drain err=%v\n", err != nil)
	if err == nil {
		// Drain returns nil when its walk reached the end, even when the parse
		// was stopped while it read the last body; a later scope reports that
		// stop. ProfileStrict frames what follows a multipart again, as it
		// always did, so it is not asked. Anything else must find nothing
		// left to read.
		if p.cfg.opts.Profile != message.ProfileStrict {
			if again := e.Drain(); again != nil && !skSame(again, e.ParseStop()) {
				return fmt.Errorf("Drain after a Drain that ended at the end returned %v", again)
			}
		}
		p.complete = true
	}
	return nil
}

func (p *skPass) walk(e *message.Entity) error {
	var fail error
	var carriers []*message.Entity
	calls := 0
	err := e.Walk(func(path []int, ent *message.Entity, werr error) error {
		if calls++; calls > skMaxCalls {
			fail = fmt.Errorf("Walk called walkFunc more than %d times", skMaxCalls)
			return fail
		}
		if ent == nil {
			fail = fmt.Errorf("Walk handed walkFunc no entity at %v", path)
			return fail
		}
		mt, _, _ := ent.EffectiveContentType()
		if ent.MultipartReader() != nil {
			fmt.Fprintf(&p.out, "%v %s\n", path, mt)
			return nil
		}
		if p.cfg.opts.WalkEmbeddedMessages && ent.CarriesMessage() {
			fmt.Fprintf(&p.out, "%v %s\n", path, mt)
			carriers = append(carriers, ent)
			return nil
		}
		n, sum, rerr := p.body(ent.Body)
		if p.decoded > p.bound() {
			fail = rerr
			return fail
		}
		fmt.Fprintf(&p.out, "%v %s %d %x err=%v warn=%v\n", path, mt, n, sum[:4], rerr != nil, werr != nil)
		return nil
	})
	if fail != nil {
		return fail
	}
	// What an embedded message left of its carrier's body is read too, as
	// ReadTree and Drain read it.
	for _, c := range carriers {
		if _, _, rerr := p.body(c.Body); p.decoded > p.bound() {
			return rerr
		}
	}
	fmt.Fprintf(&p.out, "walk err=%v\n", err != nil)
	p.complete = err == nil
	return nil
}

func (p *skPass) reader(e *message.Entity) error {
	mr := mail.NewReader(e)
	defer mr.Close()
	var last error
	refused := false
	for calls := 0; ; calls++ {
		if calls > skMaxCalls {
			return fmt.Errorf("mail.Reader.NextPart neither ended nor repeated its error in %d calls", skMaxCalls)
		}
		part, err := mr.NextPart()
		if err == io.EOF {
			// A part returned as an error alone was not read.
			p.complete = !refused
			break
		}
		if part == nil {
			if err == nil {
				return errors.New("mail.Reader.NextPart returned no part and no error")
			}
			refused = true
			fmt.Fprintf(&p.out, "part err fatal=%v\n", message.IsFatal(err))
			if last != nil && (skSame(last, err) || last.Error() == err.Error()) {
				break
			}
			last = err
			continue
		}
		last = nil
		kind := "inline"
		if _, ok := part.Header.(*mail.AttachmentHeader); ok {
			kind = "attachment"
		}
		n, sum, rerr := p.body(part.Body)
		if p.decoded > p.bound() {
			return rerr
		}
		fmt.Fprintf(&p.out, "%s %d %x err=%v warn=%v\n", kind, n, sum[:4], rerr != nil, err != nil)
	}
	fmt.Fprintf(&p.out, "reader end complete=%v\n", p.complete)
	return nil
}

// skSame compares two errors without calling anything on them.
func skSame(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

func (p *skPass) tree(e *message.Entity) error {
	opts := p.cfg.tree
	var (
		sp    *mail.Spool
		store *skStore
	)
	if p.cfg.mode == skModeSpool {
		so := mail.SpoolOptions{MemoryBytes: p.cfg.spoolMem, DiskBytes: p.cfg.spoolDisk, Dir: p.spoolDir}
		if p.cfg.store {
			store = skNewStore(p.cfg.storeFail)
			so.Store = store
		}
		sp = mail.NewSpool(&so)
		defer sp.Close()
		opts.Body = sp.Body
	}
	tr, terr := mail.ReadTree(e, &opts)
	if tr == nil || tr.Root == nil {
		return fmt.Errorf("ReadTree returned no tree (err %v)", terr)
	}
	fmt.Fprintf(&p.out, "tree err=%v\n", terr != nil)
	p.complete = terr == nil

	var total int64
	var leaves []*mail.Node
	var check func(n *mail.Node, depth int) error
	check = func(n *mail.Node, depth int) error {
		if depth > 256 {
			return errors.New("the tree is deeper than 256")
		}
		for i, c := range n.Children {
			if c == nil || c.Parent != n {
				return fmt.Errorf("child %d of %v does not name it as its parent", i, n.Path)
			}
			if len(c.Path) != len(n.Path)+1 || !slices.Equal(c.Path[:len(n.Path)], n.Path) {
				return fmt.Errorf("child %d of %v has the path %v", i, n.Path, c.Path)
			}
		}
		// A carrier whose message could not be read is a leaf with its whole
		// body, compared like any other (seed 1300007, iteration 480).
		fmt.Fprintf(&p.out, "%v %s %s %d %x trunc=%v err=%v warn=%v dup=%v span=%v/%d-%d\n", n.Path, n.MediaType, n.Role, n.Size, n.SHA256[:4], n.Truncated, n.Err != nil, n.Warning != nil, n.DuplicateOf != nil, n.HasSpan, n.SpanStart, n.SpanEnd)
		if n.Err != nil && (len(n.Children) > 0 || n.Role == mail.RoleContainer) {
			// A part the iterator returned as an error alone was not read.
			p.complete = false
		}
		switch {
		case len(n.Children) > 0:
			if n.Size != 0 || len(n.Data) != 0 {
				return fmt.Errorf("container %v has a body of %d bytes", n.Path, n.Size)
			}
		case n.Role == mail.RoleContainer:
			// A multipart with no parts, or one read as one body.
			leaves = append(leaves, n)
			p.decoded += n.Size
			if n.Size != 0 {
				return fmt.Errorf("%v holds a body of %d bytes and is classified as a container, which views skip", n.Path, n.Size)
			}
		default:
			leaves = append(leaves, n)
			total += int64(len(n.Data))
			p.decoded += n.Size
			switch {
			case int64(len(n.Data)) > n.Size:
				return fmt.Errorf("leaf %v keeps %d bytes of a body of %d", n.Path, len(n.Data), n.Size)
			case opts.Body == nil && n.Truncated != (int64(len(n.Data)) < n.Size):
				return fmt.Errorf("leaf %v: Truncated is %v with %d of %d bytes kept", n.Path, n.Truncated, len(n.Data), n.Size)
			case opts.Body != nil && len(n.Data) != 0:
				return fmt.Errorf("leaf %v keeps data although TreeOptions.Body read it", n.Path)
			case opts.MaxBodyBytes > 0 && int64(len(n.Data)) > opts.MaxBodyBytes,
				opts.MaxBodyBytes < 0 && len(n.Data) != 0:
				return fmt.Errorf("leaf %v keeps %d bytes under MaxBodyBytes %d", n.Path, len(n.Data), opts.MaxBodyBytes)
			case terr == nil && opts.Body == nil && !n.Truncated && sha256.Sum256(n.Data) != n.SHA256:
				// A tree ReadTree returned with an error may hold a node it
				// never read, whose digest is zero.
				return fmt.Errorf("leaf %v: the digest is not that of the body kept whole", n.Path)
			case n.DuplicateOf != nil && (n.DuplicateOf == n || n.DuplicateOf.SHA256 != n.SHA256):
				return fmt.Errorf("leaf %v is a duplicate of a different body", n.Path)
			}
		}
		for _, c := range n.Children {
			if err := check(c, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(tr.Root, 0); err != nil {
		return err
	}
	if p.decoded > p.bound() {
		return fmt.Errorf("the leaves hold %d decoded bytes from %d bytes of input", p.decoded, len(p.in))
	}
	if limit := opts.MaxTotalBytes; limit > 0 && total > limit {
		return fmt.Errorf("the tree keeps %d bytes under MaxTotalBytes %d", total, limit)
	}
	for _, d := range tr.Diagnostics {
		if !skCodeFormat.MatchString(string(d.Code)) {
			return fmt.Errorf("the tree reported the malformed code %q", d.Code)
		}
		fmt.Fprintf(&p.out, "tree %s %v\n", d.Code, d.Path)
	}
	// The views a caller asks of a tree; they must answer within it.
	for _, n := range append(tr.Root.Bodies("text/plain"), tr.Root.Attachments()...) {
		if n == nil || !skUnder(tr.Root, n) {
			return errors.New("a view of the tree returned a node outside it")
		}
	}
	for _, n := range leaves {
		if res := n.Resolve("cid:logo@x"); res != nil && !skUnder(tr.Root, res) {
			return errors.New("Resolve returned a node outside the tree")
		}
		n.RelatedRoot()
	}

	if sp != nil {
		if err := p.checkSpool(sp, store, leaves, terr); err != nil {
			return err
		}
	}
	return nil
}

func skUnder(root, n *mail.Node) bool {
	for ; n != nil; n = n.Parent {
		if n == root {
			return true
		}
	}
	return false
}

// checkSpool reads back what the spool kept, closes it, and checks that it
// left nothing behind.
func (p *skPass) checkSpool(sp *mail.Spool, store *skStore, leaves []*mail.Node, terr error) error {
	mem, disk := sp.Usage()
	if p.cfg.spoolMem > 0 && mem > p.cfg.spoolMem {
		return fmt.Errorf("the spool holds %d bytes in memory under a quota of %d", mem, p.cfg.spoolMem)
	}
	if p.cfg.spoolDisk > 0 && disk > p.cfg.spoolDisk {
		return fmt.Errorf("the spool holds %d bytes outside memory under a quota of %d", disk, p.cfg.spoolDisk)
	}
	for _, n := range leaves {
		size, cut, ok := sp.Kept(n)
		if !ok {
			continue
		}
		if size > n.Size {
			return fmt.Errorf("the spool keeps %d bytes of the %d-byte body %v", size, n.Size, n.Path)
		}
		rc, ok, err := sp.Open(n)
		if !ok || err != nil {
			return fmt.Errorf("the spool keeps %v and cannot open it: ok=%v err=%v", n.Path, ok, err)
		}
		h := sha256.New()
		got, err := io.Copy(h, io.LimitReader(rc, size+1))
		rc.Close()
		if err != nil || got != size {
			return fmt.Errorf("the spool keeps %d bytes of %v and gave back %d (err %v)", size, n.Path, got, err)
		}
		if !cut && n.Err == nil && (size != n.Size || [sha256.Size]byte(h.Sum(nil)) != n.SHA256) {
			return fmt.Errorf("the spool kept %v whole, %d bytes, and the tree says %d bytes or another digest", n.Path, size, n.Size)
		}
	}
	for _, d := range sp.Diagnostics() {
		if d.Code != message.CodeLimitDiskSpoolBytes {
			return fmt.Errorf("the spool reported %q", d.Code)
		}
	}
	if err := sp.Close(); err != nil {
		return fmt.Errorf("Spool.Close: %v", err)
	}
	if err := sp.Close(); err != nil {
		return fmt.Errorf("a second Spool.Close: %v", err)
	}
	if mem, disk := sp.Usage(); mem != 0 || disk != 0 {
		return fmt.Errorf("a closed spool still holds %d and %d bytes", mem, disk)
	}
	for _, n := range leaves {
		if _, ok, _ := sp.Open(n); ok {
			return fmt.Errorf("a closed spool still opens %v", n.Path)
		}
	}
	ents, err := os.ReadDir(p.spoolDir)
	if err != nil {
		return err
	}
	if len(ents) != 0 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
			os.Remove(filepath.Join(p.spoolDir, e.Name()))
		}
		return fmt.Errorf("Spool.Close left %v behind (ReadTree err %v)", names, terr)
	}
	if store != nil {
		return store.check()
	}
	return nil
}

// check holds the session to what it reports once the consumer is done.
func (p *skPass) check(s *message.ParseSession, sink *skSink) error {
	c := s.Completion()
	p.state = c.State
	switch {
	case c.State.String() == "unknown":
		return fmt.Errorf("unknown completion state %d", c.State)
	case c.Entities < 0, c.Bodies < 0, c.Multiparts < 0, c.BodiesRead < 0, c.MultipartsRead < 0:
		return fmt.Errorf("negative completion counters: %+v", c)
	case c.BodiesRead > c.Bodies, c.MultipartsRead > c.Multiparts:
		return fmt.Errorf("more read than there is: %+v", c)
	case c.Bodies+c.Multiparts > c.Entities:
		return fmt.Errorf("more bodies and multiparts than entities: %+v", c)
	case c.InputBytes > int64(len(p.in)):
		return fmt.Errorf("%d input bytes counted from an input of %d", c.InputBytes, len(p.in))
	case c.Clean() && c.State != message.CompletionComplete:
		return fmt.Errorf("clean in state %v", c.State)
	}
	if l := p.cfg.opts.Limits.MaxInputBytes; l > 0 && c.InputBytes > l+1 {
		return fmt.Errorf("%d input bytes read under MaxInputBytes %d", c.InputBytes, l)
	}
	if p.hostile {
		if p.cfg.hostile == skHostileIOError && c.InputErr != nil && !skSame(c.InputErr, skErrIO) {
			return fmt.Errorf("the input failed with %v and Completion reports %v", skErrIO, c.InputErr)
		}
	} else if c.InputErr != nil {
		return fmt.Errorf("the input never failed, and Completion reports %v", c.InputErr)
	}

	var produced int64
	for sev, n := range c.Diagnostics {
		if n < 0 {
			return fmt.Errorf("%d diagnostics of severity %d", n, sev)
		}
		produced += n
	}
	diags := s.Diagnostics()
	if int64(len(diags)) > produced {
		return fmt.Errorf("%d diagnostics kept and %d produced", len(diags), produced)
	}
	if limit := s.Limits().MaxDiagnosticCount; limit > 0 {
		if int64(len(diags)) > limit {
			return fmt.Errorf("%d diagnostics kept under MaxDiagnosticCount %d", len(diags), limit)
		}
		if sink != nil && int64(sink.calls) > limit {
			return fmt.Errorf("the sink was called %d times under MaxDiagnosticCount %d", sink.calls, limit)
		}
	}
	for _, d := range diags {
		if !skCodeFormat.MatchString(string(d.Code)) {
			return fmt.Errorf("malformed code %q", d.Code)
		}
		if d.Severity > message.SeverityFatal {
			return fmt.Errorf("%s has the severity %d", d.Code, d.Severity)
		}
		if slices.ContainsFunc(d.Path, func(i int) bool { return i < 0 }) {
			return fmt.Errorf("%s has the path %v", d.Code, d.Path)
		}
		if d.Location.HasOffset && (d.Location.Offset < 0 || d.Location.EndOffset < 0) {
			return fmt.Errorf("%s at %v", d.Code, d.Location)
		}
		p.codes = append(p.codes, d.Code)
	}
	if sink != nil {
		if p.cfg.discard {
			if len(diags) != 0 {
				return fmt.Errorf("%d diagnostics kept although the sink was given them to discard", len(diags))
			}
		} else if sink.calls != len(diags) && !(sink.stopped && sink.calls < len(diags)) {
			// A sink that stopped the parse is not called again, while the
			// session keeps what is still recorded.
			return fmt.Errorf("the sink was given %d diagnostics and the session kept %d", sink.calls, len(diags))
		}
		if sink.stopped && c.State != message.CompletionStopped && c.State != message.CompletionCanceled {
			return fmt.Errorf("the sink stopped the parse and the state is %v", c.State)
		}
	}

	// A consumer that read everything leaves nothing in progress, unless the
	// parse was stopped or its input failed.
	if p.complete && !p.hostile && c.Stop == nil {
		if c.BodiesRead != c.Bodies || c.MultipartsRead != c.Multiparts {
			return fmt.Errorf("the %s consumer read everything and the session counts %d of %d bodies and %d of %d multiparts read (%v)",
				skModeNames[p.cfg.mode], c.BodiesRead, c.Bodies, c.MultipartsRead, c.Multiparts, c.State)
		}
		if c.State == message.CompletionNotConsumed || c.State == message.CompletionInProgress {
			return fmt.Errorf("the %s consumer read everything and the state is %v", skModeNames[p.cfg.mode], c.State)
		}
	}

	fmt.Fprintf(&p.out, "state=%v entities=%d bodies=%d/%d multiparts=%d/%d ambiguous=%v stop=%v\n",
		c.State, c.Entities, c.BodiesRead, c.Bodies, c.MultipartsRead, c.Multiparts, c.Ambiguous, c.Stop != nil)
	// In the order they were recorded in, which is part of the outcome: a
	// transfer decoder reports a repair when its consumer has read up to it,
	// and a carrier's body is read in whole blocks, so the order does not
	// depend on the chunking (see TestSoak_diagnosticOrder).
	var lines []string
	if sink != nil && p.cfg.discard {
		lines = append(lines, sink.seen...)
	}
	for _, d := range diags {
		lines = append(lines, fmt.Sprintf("%s %s %v", d.Severity, d.Code, d.Path))
	}
	for _, l := range lines {
		p.out.WriteString(l + "\n")
	}
	return nil
}

// ---------------------------------------------------------------------------
// The generator

type skGen struct {
	r        *rand.Rand
	fixtures [][]byte
	eolMode  int
}

func skPick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

func (g *skGen) pick(xs ...string) string { return skPick(g.r, xs...) }

// chance is true one time in n.
func (g *skGen) chance(n int) bool { return g.r.IntN(n) == 0 }

func (g *skGen) eol() string {
	switch g.eolMode {
	case 0:
		return "\r\n"
	case 1:
		return "\n"
	}
	return g.pick("\r\n", "\r\n", "\n", "\r")
}

func (g *skGen) input() []byte {
	var b []byte
	if g.r.IntN(20) < 9 {
		b = bytes.Clone(g.fixtures[g.r.IntN(len(g.fixtures))])
		if !g.chance(20) {
			b = g.damage(b, 1+g.r.IntN(4))
		}
	} else {
		b = g.structured()
	}
	return skCut(b, skMaxInput)
}

// ---- mutations

var skTokens = []string{
	"\r\n", "\n", "\r", "\r\n\r\n", "--", "--B\r\n", "--B--\r\n", ":", " ", "\t", "=", "=?", "?=", "=?utf-8?b?",
	"=?utf-8?q?=FF?=", "Content-Type: multipart/mixed; boundary=B\r\n", "Content-Transfer-Encoding: base64\r\n",
	"Content-Type: message/rfc822\r\n", "begin 644 x\n", "end\n", "\xef\xbb\xbf", "\xff\xfe", "\x00",
	"From x@example.com Thu Jan  1 00:00:00 2026\n", ";", "\"", "boundary=", "charset=euc-kr", "filename*0*=utf-8''%ED%95%9C",
	"<", ">", "\x1b$B", "\xc7\xd1", "==", "=\r\n",
}

// damage applies rounds of random mutations, each cut to the input cap.
func (g *skGen) damage(b []byte, rounds int) []byte {
	for range rounds {
		switch g.r.IntN(10) {
		case 0: // flip a bit
			if len(b) > 0 {
				b[g.r.IntN(len(b))] ^= 1 << g.r.IntN(8)
			}
		case 1: // overwrite a byte with one the parser cares about
			if len(b) > 0 {
				const special = "\r\n-:= ;\"\x00\xff<>"
				b[g.r.IntN(len(b))] = special[g.r.IntN(len(special))]
			}
		case 2: // delete a range
			if len(b) > 0 {
				i := g.r.IntN(len(b))
				j := min(len(b), i+1+g.r.IntN(16))
				b = append(b[:i:i], b[j:]...)
			}
		case 3: // duplicate a line
			lines := skLines(b)
			l := lines[g.r.IntN(len(lines))]
			line := bytes.Clone(b[l[0]:l[1]])
			rep := bytes.Repeat(line, 1+g.r.IntN(3))
			b = slices.Insert(b, l[1], rep...)
		case 4: // truncate
			if len(b) > 0 {
				b = b[:g.r.IntN(len(b))]
			}
		case 5:
			b = g.rewriteEOL(b)
		case 6: // splice with another fixture
			o := g.fixtures[g.r.IntN(len(g.fixtures))]
			i, j := g.r.IntN(len(b)+1), g.r.IntN(len(o)+1)
			b = append(b[:i:i], o[j:]...)
		case 7: // insert a token
			i := g.r.IntN(len(b) + 1)
			b = slices.Insert(b, i, []byte(skTokens[g.r.IntN(len(skTokens))])...)
		case 8: // swap two lines
			lines := skLines(b)
			if len(lines) > 1 {
				x, y := lines[g.r.IntN(len(lines))], lines[g.r.IntN(len(lines))]
				if x[0] > y[0] {
					x, y = y, x
				}
				if x[1] <= y[0] {
					var nb []byte
					nb = append(nb, b[:x[0]]...)
					nb = append(nb, b[y[0]:y[1]]...)
					nb = append(nb, b[x[1]:y[0]]...)
					nb = append(nb, b[x[0]:x[1]]...)
					nb = append(nb, b[y[1]:]...)
					b = nb
				}
			}
		case 9: // repeat a short range many times
			if len(b) > 0 {
				i := g.r.IntN(len(b))
				j := min(len(b), i+1+g.r.IntN(8))
				rep := bytes.Repeat(b[i:j], 1+g.r.IntN(512))
				b = slices.Insert(b, j, rep...)
			}
		}
		b = skCut(b, skMaxInput)
	}
	return b
}

// skLines returns the [start, end) of every line of b, its ending included;
// an empty b has one empty line.
func skLines(b []byte) [][2]int {
	var out [][2]int
	start := 0
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\n':
			out = append(out, [2]int{start, i + 1})
			start = i + 1
		case '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			out = append(out, [2]int{start, i + 1})
			start = i + 1
		}
	}
	if start < len(b) || len(out) == 0 {
		out = append(out, [2]int{start, len(b)})
	}
	return out
}

// rewriteEOL rewrites every line ending of b in one style, or each at
// random.
func (g *skGen) rewriteEOL(b []byte) []byte {
	mode := g.r.IntN(4)
	var out []byte
	for _, l := range skLines(b) {
		line := b[l[0]:l[1]]
		content := bytes.TrimRight(line, "\r\n")
		out = append(out, content...)
		if len(content) == len(line) {
			continue
		}
		switch mode {
		case 0:
			out = append(out, "\r\n"...)
		case 1:
			out = append(out, '\n')
		case 2:
			out = append(out, '\r')
		default:
			out = append(out, g.pick("\r\n", "\n", "\r", "\n\r")...)
		}
	}
	return out
}

// ---- the structured generator

func skB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

var (
	skEUCKR    = "\xbe\xc8\xb3\xe7\xc7\xcf\xbc\xbc\xbf\xe4" // 안녕하세요
	skISO2022  = "\x1b$B$3$s$K$A$O\x1b(B"                   // こんにちは
	skCP1252   = "caf\xe9 \x93quoted\x94 \x80"
	skSnippets = map[string][]string{
		"utf-8":        {"안녕하세요", "café", "✓ ok", "日本語", "한글 메일"},
		"euc-kr":       {skEUCKR, "\xc7\xd1\xb1\xdb"},
		"iso-2022-jp":  {skISO2022, "\x1b$B$\"\x1b(B"},
		"windows-1252": {skCP1252, "\x85\x96"},
	}
	skAddrs = []string{
		`"Alice Example" <alice@example.com>`, "bob@example.org", "undisclosed-recipients:;",
		"=?utf-8?b?" + skB64("홍길동") + "?= <hong@example.kr>", "=?euc-kr?b?" + skB64(skEUCKR) + "?= <kim@example.kr>",
		`"broken <a@example.com`, "<<x@example.com>>", "a@example.com, c@example.com,, e@example.com",
		"Group: a@example.com, b@example.com;", "(comment) user@[192.0.2.1]", "@", "\"\xff\xfe\" <raw@example.com>",
	}
	skSubjects = []string{
		"Hello there", "=?utf-8?q?caf=C3=A9_au_lait?=", "=?utf-8?b?" + skB64("안녕하세요 세계") + "?=",
		"=?euc-kr?b?" + skB64(skEUCKR) + "?=", "=?iso-2022-jp?b?" + skB64(skISO2022) + "?=", "=?x-made-up?q?abc?=",
		"=?utf-8?b?###?=", "=?utf-8?q?unterminated", "Re: =?utf-8?b?" + skB64("한") + "?= =?utf-8?b?" + skB64("글") + "?=",
		"raw 8-bit 안녕", strings.Repeat("=?utf-8?q?a?= ", 40), "=?utf-8*ko?q?x?=", "=?utf-8?b?" + skB64("\xff") + "?=",
	}
	skDates = []string{
		"Thu, 01 Jan 2026 00:00:00 +0000", "1 Jan 2026 00:00 GMT", "Thu, 31 Feb 2026 25:61:00 +9999", "yesterday",
		"Mon, 1 Jan 1900 00:00:00 -0000 (UTC)", "", "Fri, 13 Mar 2026 12:00:00 KST",
	}
	skMsgIDs    = []string{"<abc.123@example.com>", "abc@example.com", "<a@example.com> <c@example.com>", "<>", "<\"q\"@x>"}
	skFilenames = []string{
		"a.txt", `"report.pdf"`, "\"=?utf-8?b?" + skB64("문서.hwp") + "?=\"", `"../../etc/passwd"`, `"con.txt"`,
		"\"a\x00b\"", `"unterminated`, "invoice.pdf.exe",
	}
	skLeafTypes = []string{
		"text/plain", "text/plain", "text/html", "application/octet-stream", "image/png", "application/pdf",
		"text/calendar", "message/delivery-status", "application/pkcs7-signature", "text", "application/x-unknown",
		"text/plain/extra", "TEXT/PLAIN",
	}
	skCharsets = []string{
		"utf-8", "euc-kr", "iso-2022-jp", "windows-1252", "us-ascii", "x-made-up", "UTF-8", "ks_c_5601-1987",
		`"utf-8"`, "\"euc-kr", "unicode-1-1-utf-7", "",
	}
	skCTEs = []string{
		"", "7bit", "8bit", "binary", "base64", "base64", "quoted-printable", "quoted-printable", "x-uuencode",
		"uuencode", "x-foo", "Base64", "base64 junk", "quoted-printable; x=y", "7-bit",
	}
	skWords = []string{
		"lorem", "ipsum", "free", "offer", "click", "http://example.com/x?a=1", "=", "From", ".", "<html>",
		"<a href=\"cid:logo@x\">", "unsubscribe", "begin", "--", "=20", "=3D",
	}
)

func (g *skGen) structured() []byte {
	g.eolMode = g.r.IntN(3)
	var w bytes.Buffer
	if g.chance(30) {
		w.WriteString("From sender@example.com Thu Jan  1 00:00:00 2026" + g.eol())
	}
	if g.chance(40) {
		w.WriteString("\xef\xbb\xbf")
	}
	g.entity(&w, 0, true, "")
	b := w.Bytes()
	if g.chance(2) {
		b = g.damage(b, 1+g.r.IntN(3))
	}
	return b
}

func (g *skGen) field(w *bytes.Buffer, name, value string) {
	if g.chance(5) {
		if i := strings.IndexByte(value, ' '); i > 0 {
			value = value[:i] + g.eol() + g.pick(" ", "\t") + value[i+1:]
		}
	}
	sep := ": "
	if g.chance(20) {
		sep = g.pick(":", " : ", ":\t", "::")
	}
	w.WriteString(name + sep + value + g.eol())
}

// blank ends a header block, or does not.
func (g *skGen) blank(w *bytes.Buffer) {
	switch {
	case g.chance(30):
	case g.chance(30):
		w.WriteString(" " + g.eol())
	default:
		w.WriteString(g.eol())
	}
}

func (g *skGen) topHeaders(w *bytes.Buffer) {
	if g.chance(25) {
		w.WriteString(" leading continuation" + g.eol())
	}
	gens := []func(){
		func() { g.field(w, "From", g.pick(skAddrs...)) },
		func() { g.field(w, "To", g.pick(skAddrs...)) },
		func() { g.field(w, "Cc", g.pick(skAddrs...)+", "+g.pick(skAddrs...)) },
		func() { g.field(w, "Subject", g.pick(skSubjects...)) },
		func() { g.field(w, "Date", g.pick(skDates...)) },
		func() { g.field(w, "Message-ID", g.pick(skMsgIDs...)) },
		func() { g.field(w, "MIME-Version", g.pick("1.0", "1.0", "1.0 (generated)", "2.0", "")) },
		func() {
			g.field(w, "Received", "from mx.example.org (mx.example.org [192.0.2.1]) by mail.example.com with ESMTP id abc; "+g.pick(skDates...))
		},
		func() { g.field(w, "Reply-To", g.pick(skAddrs...)) },
		func() {
			g.field(w, "List-Unsubscribe", "<mailto:u@example.com?subject=unsub>, <https://example.com/u?id=1>")
		},
		func() {
			g.field(w, "Authentication-Results", "mx.example.com; spf=pass smtp.mailfrom=example.org; dkim=fail header.d=example.org")
		},
		func() { g.field(w, "In-Reply-To", g.pick(skMsgIDs...)) },
		func() { g.field(w, "X-Mailer", "soak \xff\xfe 8bit") },
	}
	for range g.r.IntN(9) {
		gens[g.r.IntN(len(gens))]()
	}
	if g.chance(12) {
		w.WriteString("this line has no colon" + g.eol())
	}
	if g.chance(25) {
		w.WriteString("X-Long: " + strings.Repeat("a", g.r.IntN(3000)) + g.eol())
	}
	if g.chance(20) {
		w.WriteString("Bad Name: x" + g.eol())
	}
}

func (g *skGen) entity(w *bytes.Buffer, depth int, top bool, parentB string) {
	if top {
		g.topHeaders(w)
	}
	kind := 0
	if depth < 4 && w.Len() < skMaxInput/2 {
		x := g.r.IntN(20)
		if depth == 0 {
			x -= 3
		}
		switch {
		case x < 7:
			kind = 1
		case x < 9:
			kind = 2
		case x < 10:
			kind = 3
		}
	}
	switch kind {
	case 1:
		g.multipart(w, depth, parentB)
	case 2:
		g.embedded(w, depth)
	case 3:
		g.field(w, "Content-Type", g.pick("text/rfc822-headers", "message/rfc822-headers"))
		g.blank(w)
		g.topHeaders(w)
	default:
		g.leaf(w)
	}
}

func (g *skGen) boundary() string {
	switch g.r.IntN(6) {
	case 0:
		return "B"
	case 1:
		return "----=_Part_" + strconv.Itoa(g.r.IntN(100000))
	case 2:
		return "=_" + strconv.FormatUint(g.r.Uint64(), 16)
	}
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz'()+_,-./:=? "
	b := make([]byte, 1+g.r.IntN(70))
	for i := range b {
		b[i] = chars[g.r.IntN(len(chars))]
	}
	return strings.TrimRight(string(b), " ") + "x"
}

func (g *skGen) multipart(w *bytes.Buffer, depth int, parentB string) {
	sub := g.pick("mixed", "mixed", "alternative", "related", "signed", "report", "digest", "encrypted", "x-unknown", "parallel")
	b := g.boundary()
	declared := b
	switch g.r.IntN(10) {
	case 0:
		declared = "" // no boundary parameter
	case 1:
		declared = g.boundary() // the wrong one
	case 2:
		if parentB != "" {
			b, declared = parentB, parentB // reused
		}
	}
	v := "multipart/" + sub
	if declared != "" {
		sep := "; "
		if g.chance(4) {
			sep = ";" + g.eol() + "\t"
		}
		switch {
		case g.chance(8):
			v += sep + "boundary*0=\"" + declared[:len(declared)/2] + "\"; boundary*1=\"" + declared[len(declared)/2:] + "\""
		case g.chance(6):
			v += sep + "boundary=" + declared // unquoted, needed or not
		default:
			v += sep + "boundary=\"" + declared + "\""
		}
	}
	switch sub {
	case "related":
		v += "; type=\"text/html\"; start=\"<root@x>\""
	case "signed":
		v += "; protocol=\"application/pgp-signature\"; micalg=pgp-sha256"
	case "report":
		v += "; report-type=delivery-status"
	}
	g.field(w, "Content-Type", v)
	if g.chance(10) {
		g.field(w, "Content-Type", "multipart/mixed; boundary=\""+g.boundary()+"\"")
	}
	if g.chance(15) {
		g.field(w, "Content-Transfer-Encoding", g.pick("base64", "quoted-printable", "8bit"))
	}
	g.blank(w)
	if g.chance(3) {
		w.WriteString("This is a multi-part message in MIME format." + g.eol())
	}
	for range g.r.IntN(5) {
		if w.Len() > skMaxInput {
			break
		}
		w.WriteString("--" + b + g.pick("", "", "", " ", "\t", " junk") + g.eol())
		g.entity(w, depth+1, false, b)
		w.WriteString(g.eol())
	}
	switch g.r.IntN(8) {
	case 0: // never closed
	case 1:
		w.WriteString("--" + b + "-" + g.eol())
	case 2:
		w.WriteString("--" + b + "--junk" + g.eol())
	default:
		w.WriteString("--" + b + "--")
		if !g.chance(4) {
			w.WriteString(g.eol())
		}
	}
	if g.chance(4) {
		w.WriteString("epilogue" + g.eol())
	}
}

func (g *skGen) embedded(w *bytes.Buffer, depth int) {
	g.field(w, "Content-Type", g.pick("message/rfc822", "message/rfc822", "message/global", "message/news"))
	cte := g.pick("", "", "7bit", "8bit", "base64", "quoted-printable", "binary")
	if cte != "" {
		g.field(w, "Content-Transfer-Encoding", cte)
	}
	g.blank(w)
	var inner bytes.Buffer
	g.entity(&inner, depth+1, true, "")
	w.Write(g.encode(inner.Bytes(), cte))
}

func (g *skGen) leaf(w *bytes.Buffer) {
	ct := g.pick(skLeafTypes...)
	cs := ""
	if strings.HasPrefix(strings.ToLower(ct), "text") || g.chance(10) {
		cs = g.pick(skCharsets...)
	}
	content := g.content(cs)
	cte := g.pick(skCTEs...)
	if !g.chance(6) {
		v := ct
		if cs != "" {
			v += "; charset=" + cs
		}
		if g.chance(4) {
			v += "; name=" + g.pick(skFilenames...)
		}
		if g.chance(12) {
			v += g.pick("; broken", "; a=1; a=2", ";;", "; charset=utf-8", "; =x")
		}
		g.field(w, "Content-Type", v)
	}
	if cte != "" {
		g.field(w, "Content-Transfer-Encoding", cte)
	}
	if g.chance(3) {
		g.field(w, "Content-Disposition", g.pick("attachment", "inline", "attachment; filename="+g.pick(skFilenames...),
			"attachment; filename*=utf-8''%ED%95%9C%EA%B8%80.txt", "attachment; filename*0=\"a\"; filename*1=\"b.txt\"",
			"inline; filename=\"unterminated", "form-data; name=x", "attachment; filename=a.txt; filename=b.txt"))
	}
	if g.chance(6) {
		g.field(w, "Content-ID", "<logo@x>")
	}
	if g.chance(10) {
		g.field(w, "Content-Location", "http://example.com/a.gif")
	}
	g.blank(w)
	w.Write(g.encode(content, cte))
}

func (g *skGen) content(cs string) []byte {
	n := g.r.IntN(400)
	switch {
	case g.chance(10):
		n = g.r.IntN(8 << 10)
	case g.chance(60):
		n = g.r.IntN(48 << 10)
	}
	key := strings.ToLower(strings.Trim(cs, `"`))
	switch key {
	case "ks_c_5601-1987":
		key = "euc-kr"
	case "", "us-ascii", "x-made-up", "unicode-1-1-utf-7":
		key = g.pick("utf-8", "euc-kr", "windows-1252")
	}
	var b bytes.Buffer
	for b.Len() < n {
		switch g.r.IntN(10) {
		case 0:
			for range 1 + g.r.IntN(16) {
				b.WriteByte(byte(g.r.UintN(256)))
			}
		case 1, 2:
			b.WriteString(g.pick(skSnippets[key]...))
		case 3:
			b.WriteString(g.eol())
		case 4:
			if g.chance(8) {
				b.WriteString(g.eol() + "--" + g.boundary() + g.eol())
			} else {
				b.WriteString(g.pick(skWords...) + " ")
			}
		default:
			b.WriteString(g.pick(skWords...) + " ")
		}
	}
	return b.Bytes()
}

func (g *skGen) encode(content []byte, cte string) []byte {
	name := ""
	if f := strings.Fields(strings.ToLower(cte)); len(f) > 0 {
		name = strings.TrimRight(f[0], ";")
	}
	switch name {
	case "base64":
		return g.base64(content)
	case "quoted-printable":
		return g.qp(content)
	case "x-uuencode", "uuencode":
		return g.uuencode(content)
	}
	return content
}

func (g *skGen) base64(content []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(content)
	if g.chance(6) {
		enc = strings.TrimRight(enc, "=")
	}
	lineLen := 76
	switch g.r.IntN(6) {
	case 0:
		lineLen = 0
	case 1:
		lineLen = 1 + g.r.IntN(100)
	}
	var b bytes.Buffer
	for i := 0; i < len(enc); {
		n := len(enc) - i
		if lineLen > 0 {
			n = min(n, lineLen)
		}
		b.WriteString(enc[i : i+n])
		i += n
		if i < len(enc) || g.chance(2) {
			b.WriteString(g.eol())
		}
		if g.chance(40) {
			b.WriteString(g.pick("!!", "*", " ", "\t", "=", "==", "-", "\x00", "ü"))
		}
	}
	if g.chance(10) {
		b.WriteString("=" + g.eol() + "data after the padding" + g.eol())
	}
	return b.Bytes()
}

func (g *skGen) qp(content []byte) []byte {
	var b bytes.Buffer
	qw := quotedprintable.NewWriter(&b)
	qw.Write(content)
	qw.Close()
	out := b.Bytes()
	if g.eolMode == 1 {
		out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	}
	for range g.r.IntN(4) {
		i := g.r.IntN(len(out) + 1)
		out = slices.Insert(out, i, []byte(g.pick("=ZZ", "=\r", "=", "=4", "= \r\n", "=\n", strings.Repeat("x", 1200)))...)
	}
	return out
}

func (g *skGen) uuencode(content []byte) []byte {
	var b bytes.Buffer
	enc := func(v byte) byte {
		if v == 0 {
			return '`'
		}
		return ' ' + v
	}
	if !g.chance(8) {
		b.WriteString("begin 644 " + g.pick("a.txt", "file name.bin", "") + g.eol())
	}
	for i := 0; i < len(content); i += 45 {
		chunk := content[i:min(i+45, len(content))]
		line := []byte{enc(byte(len(chunk)))}
		for j := 0; j < len(chunk); j += 3 {
			var x [3]byte
			copy(x[:], chunk[j:])
			line = append(line, enc(x[0]>>2), enc((x[0]<<4|x[1]>>4)&63), enc((x[1]<<2|x[2]>>6)&63), enc(x[2]&63))
		}
		if g.chance(30) {
			line = line[:1+g.r.IntN(len(line))]
		}
		b.Write(line)
		b.WriteString(g.eol())
	}
	if !g.chance(8) {
		b.WriteString("`" + g.eol() + "end" + g.eol())
	}
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// What the soak found

// skFailingReader delivers r and then fails, every time it is read again.
type skFailingReader struct{ r io.Reader }

func (f *skFailingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		err = skErrIO
	}
	return n, err
}

// skWithin runs f and reports whether it returned within d.
func skWithin(d time.Duration, f func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// TestSoak_readTreeEnds pins the defect the soak found first (seed 1300007,
// iteration 67): ReadTree never returned when a multipart's iterator failed
// for good, because the iterator returned the failure as a new value on every
// call, and ReadTree goes on past a part-less failure until the iterator
// repeats itself. A multipart with no boundary under ProfileStrict or Read, and
// an input that fails inside a multipart under Parse, now end the tree.
func TestSoak_readTreeEnds(t *testing.T) {
	const noBoundary = "Content-Type: multipart/mixed\r\n\r\n--B\r\n\r\nx\r\n--B--\r\n"
	const unfinished = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nx\r\n--B\r\n\r\ny"
	tests := []struct {
		name  string
		parse func() (*message.Entity, error)
	}{
		{"no boundary, strict", func() (*message.Entity, error) {
			_, e, err := message.Parse(strings.NewReader(noBoundary), &message.ParseOptions{Profile: message.ProfileStrict})
			return e, err
		}},
		{"no boundary, Read", func() (*message.Entity, error) {
			return message.Read(strings.NewReader(noBoundary))
		}},
		{"failing input, tolerant", func() (*message.Entity, error) {
			_, e, err := message.Parse(&skFailingReader{strings.NewReader(unfinished)}, nil)
			return e, err
		}},
		{"failing input, forensic", func() (*message.Entity, error) {
			_, e, err := message.Parse(&skFailingReader{strings.NewReader(unfinished)}, &message.ParseOptions{Profile: message.ProfileForensic})
			return e, err
		}},
		{"failing input, strict", func() (*message.Entity, error) {
			_, e, err := message.Parse(&skFailingReader{strings.NewReader(unfinished)}, &message.ParseOptions{Profile: message.ProfileStrict})
			return e, err
		}},
		// Seed 1300007, iteration 4621: the multipart of an embedded message
		// is framed from its carrier's body, which a body budget ends.
		{"carrier past its body budget", func() (*message.Entity, error) {
			in := "Content-Type: message/rfc822\r\n\r\n" + unfinished + strings.Repeat("z", 200) + "\r\n--B\r\n\r\nlast\r\n"
			_, e, err := message.Parse(strings.NewReader(in), &message.ParseOptions{Profile: message.ProfileStrict, Limits: message.Limits{MaxDecodedBodyBytes: 120}})
			if e == nil {
				return nil, err
			}
			return e.EmbeddedMessage()
		}},
		// Seed 1300007, iteration 22774: under ProfileStrict a base64
		// carrier's decoder fails on every read once it met a byte outside
		// the alphabet.
		{"strict base64 carrier", func() (*message.Entity, error) {
			inner := base64.StdEncoding.EncodeToString([]byte(unfinished + "\r\n--B\r\n\r\nlast\r\n--B--\r\n"))
			in := "Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n" + inner[:80] + "!!!!" + inner[80:] + "\r\n"
			_, e, err := message.Parse(strings.NewReader(in), &message.ParseOptions{Profile: message.ProfileStrict})
			if e == nil {
				return nil, err
			}
			return e.EmbeddedMessage()
		}},
		// An entity from Read, whose source keeps failing inside a multipart:
		// Read does not make the source's error final, and the iterator
		// reads it again on every call.
		{"failing input, Read", func() (*message.Entity, error) {
			return message.Read(&skFailingReader{strings.NewReader(unfinished)})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr *mail.Tree
			if !skWithin(10*time.Second, func() {
				e, err := tt.parse()
				if e == nil {
					t.Errorf("no entity: %v", err)
					return
				}
				tr, _ = mail.ReadTree(e, nil)
			}) {
				t.Fatal("ReadTree did not return")
			}
			if tr == nil || tr.Root == nil || tr.Root.Err == nil {
				t.Fatalf("the multipart node records no failure: %+v", tr)
			}

			// The iterator says it has ended by repeating its failure.
			e, _ := tt.parse()
			mr := e.MultipartReader()
			var errs []error
			for range 4 {
				if p, err := mr.NextPart(); p == nil {
					errs = append(errs, err)
				} else {
					io.Copy(io.Discard, p.Body)
				}
			}
			if n := len(errs); n < 2 || errs[n-1] != errs[n-2] {
				t.Errorf("the iterator does not repeat its failure: %v", errs)
			}
		})
	}
}

// skDrainOutcome parses in under opts, drains it twice, and renders what the
// session reports.
func skDrainOutcome(t *testing.T, src io.Reader, opts *message.ParseOptions) string {
	t.Helper()
	s, e, err := message.Parse(src, opts)
	if e == nil {
		t.Fatalf("no entity: %v", err)
	}
	var b strings.Builder
	for i := range 2 {
		derr := e.Drain()
		c := s.Completion()
		fmt.Fprintf(&b, "drain %d: err=%v state=%v entities=%d bodies=%d/%d multiparts=%d/%d\n", i, derr != nil, c.State, c.Entities, c.BodiesRead, c.Bodies, c.MultipartsRead, c.Multiparts)
	}
	for _, d := range s.Diagnostics() {
		fmt.Fprintf(&b, "%s %v\n", d.Code, d.Path)
	}
	return b.String()
}

// TestSoak_drainAgain pins what seed 1300007, iteration 121 found: Drain
// called again framed the epilogue as a new multipart, healed it into a part
// that was never there (or recorded multipart.empty), and what it found
// depended on how far the first iteration had read ahead.
func TestSoak_drainAgain(t *testing.T) {
	const in = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\n--B--\r\nepilogue\r\n"
	opts := &message.ParseOptions{Profile: message.ProfileForensic}
	whole := skDrainOutcome(t, strings.NewReader(in), opts)
	want := "drain 0: err=false state=complete entities=2 bodies=1/1 multiparts=1/1\n" +
		"drain 1: err=false state=complete entities=2 bodies=1/1 multiparts=1/1\n"
	if whole != want {
		t.Errorf("draining twice:\n%s\nwant:\n%s", whole, want)
	}
	for _, n := range []int{1, 3, 16} {
		cfg := skConfig{chunkMax: n}
		if got := skDrainOutcome(t, cfg.reader([]byte(in), nil), opts); got != whole {
			t.Errorf("in chunks of %d:\n%s\nwhole:\n%s", n, got, whole)
		}
	}
}

// TestSoak_drainEmbeddedUnreadable pins what seed 1300007, iteration 91
// found: under WalkEmbeddedMessages, a carrier whose embedded message could
// not be read was left unread by Drain, and the parse stayed in progress
// after a Drain that returned nil.
func TestSoak_drainEmbeddedUnreadable(t *testing.T) {
	const in = "Content-Type: message/rfc822\r\n\r\nSubject: inner\r\nthis line has no colon\r\n\r\nbody\r\n"
	for _, profile := range []message.Profile{message.ProfileStrict, message.ProfileTolerant} {
		s, e, err := message.Parse(strings.NewReader(in), &message.ParseOptions{Profile: profile, WalkEmbeddedMessages: true})
		if e == nil {
			t.Fatalf("%v: no entity: %v", profile, err)
		}
		if err := e.Drain(); err != nil {
			t.Fatalf("%v: Drain: %v", profile, err)
		}
		if c := s.Completion(); c.BodiesRead != c.Bodies || c.State == message.CompletionInProgress {
			t.Errorf("%v: after Drain, %d of %d bodies read, state %v", profile, c.BodiesRead, c.Bodies, c.State)
		}
	}
}

// TestSoak_opaqueMultipartRole pins what seed 1300007, iteration 163 found: a
// multipart read as one body was classified as a container, which every view
// skips, whenever its Data was not kept — with a Spool, or MaxBodyBytes < 0.
func TestSoak_opaqueMultipartRole(t *testing.T) {
	const in = "Content-Type: multipart/mixed; boundary=B; boundary=C\r\n\r\n--B\r\n\r\nhidden\r\n--B--\r\n"
	for _, opts := range []*mail.TreeOptions{{MaxBodyBytes: -1}, {Body: func(*mail.Node, io.Reader) error { return nil }}} {
		_, e, _ := message.Parse(strings.NewReader(in), nil)
		if e.MultipartReader() != nil {
			t.Fatal("the multipart has a boundary it may use; the test needs one read as one body")
		}
		tr, err := mail.ReadTree(e, opts)
		if err != nil {
			t.Fatal(err)
		}
		if n := tr.Root; n.Size == 0 || n.Role == mail.RoleContainer || !slices.Contains(n.Attachments(), n) {
			t.Errorf("a %d-byte body read as one part is %v, and Attachments is %v", n.Size, n.Role, n.Attachments())
		}
	}
}

// skOpen skips a test that pins an open finding, unless EMLKIT_SOAK_OPEN is
// set, for whoever works on it.
func skOpen(t *testing.T, finding string) {
	t.Helper()
	if os.Getenv("EMLKIT_SOAK_OPEN") == "" {
		t.Skipf("open (ROB-130-007): %s; see docs/robustness-notes/soak.md, and set EMLKIT_SOAK_OPEN=1 to run it", finding)
	}
}

// skNoEmptyReads delivers r at most n bytes a read.
type skNoEmptyReads struct {
	r io.Reader
	n int
}

func (c *skNoEmptyReads) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// skDrainCodes drains the message in and returns its diagnostics.
func skDrainCodes(t *testing.T, src io.Reader, opts *message.ParseOptions) []string {
	t.Helper()
	s, e, err := message.Parse(src, opts)
	if e == nil {
		t.Fatalf("no entity: %v", err)
	}
	e.Drain()
	var out []string
	for _, d := range s.Diagnostics() {
		out = append(out, fmt.Sprintf("%s %v", d.Code, d.Path))
	}
	return append(out, s.Completion().State.String())
}

// TestSoak_rootCloseLost is what seed 1300007, iteration 5509 found. The
// message ends with the root multipart's closing delimiter. Read with a large
// buffer, the root recorded multipart.close_not_found and the parse ended
// incomplete; read a few bytes at a time, it did not. The last part's body
// filled the framing reader's buffer up to "\n--boundary-", one byte short of
// the closing delimiter, and a candidate a full buffer cut was taken for
// body, wherever in the buffer it began (textproto's scanBody; see
// TestChunkInvariance_closeAtBufferEnd there). The part framed on an inferred
// boundary before it only placed the bytes so.
func TestSoak_rootCloseLost(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "soak", "root-close-lost-after-inferred-boundary.eml"))
	if err != nil {
		t.Fatal(err)
	}
	whole := skDrainCodes(t, bytes.NewReader(in), nil)
	if slices.Contains(whole, "multipart.close_not_found []") {
		t.Errorf("read whole, the root's closing delimiter was not found:\n%q", whole)
	}
	if small := skDrainCodes(t, &skNoEmptyReads{bytes.NewReader(in), 7}, nil); !slices.Equal(whole, small) {
		t.Errorf("read 7 bytes at a time:\n%q\nwhole:\n%q", small, whole)
	}
}

// TestSoak_signedSpan is what seed 1300007, iteration 7043 found, once the
// soak got past 5509. The signed part of a multipart/signed is itself a
// multipart. ReadTree took a child's raw span as soon as the child was read,
// when a multipart child's part has been read to its end only if a buffer
// happened to reach the delimiter after it: read whole it had a span, read
// three bytes at a time it had none, and the tree said signed.unverifiable.
// The span is now taken once the parent's iterator has moved past the child.
func TestSoak_signedSpan(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("..", "testdata", "corpus", "signed-pgp-mime.eml"))
	if err != nil {
		t.Fatal(err)
	}
	spans := func(src io.Reader) string {
		_, e, perr := message.Parse(src, nil)
		if e == nil {
			t.Fatalf("no entity: %v", perr)
		}
		tr, terr := mail.ReadTree(e, nil)
		if terr != nil || len(tr.Root.Children) == 0 {
			t.Fatalf("ReadTree: %v", terr)
		}
		signed := tr.Root.Children[0]
		if len(signed.Children) == 0 || !signed.HasSpan {
			t.Errorf("the signed part %s with %d parts has no raw span", signed.MediaType, len(signed.Children))
		}
		out := fmt.Sprintf("%v %d-%d unverifiable=%v", signed.HasSpan, signed.SpanStart, signed.SpanEnd, tr.Root.Unverifiable)
		for _, d := range tr.Diagnostics {
			out += " " + string(d.Code)
		}
		return out
	}
	whole := spans(bytes.NewReader(in))
	if strings.Contains(whole, string(mail.CodeSignedUnverifiable)) {
		t.Errorf("read whole: %s", whole)
	}
	if small := spans(&skNoEmptyReads{bytes.NewReader(in), 3}); small != whole {
		t.Errorf("read 3 bytes at a time: %s; whole: %s", small, whole)
	}
}

// TestSoak_strictBase64Chunked is open, and ProfileStrict's earlier behavior:
// what seed 1300007, iterations 6478, 11189 and 19668 found. Strict decodes
// base64 with encoding/base64's streaming decoder, which refuses data after
// padding when it arrives in the same read and accepts it when a read ended
// at the padding: "aGk=\naGk=" is "hi" and an error read whole, "hihi" read a
// byte at a time.
func TestSoak_strictBase64Chunked(t *testing.T) {
	skOpen(t, "under ProfileStrict a base64 body with data after padding decodes differently with the read size")
	in, err := os.ReadFile(filepath.Join("testdata", "soak", "strict-base64-padding-chunked.eml"))
	if err != nil {
		t.Fatal(err)
	}
	body := func(src io.Reader) string {
		_, e, err := message.Parse(src, &message.ParseOptions{Profile: message.ProfileStrict})
		if e == nil {
			t.Fatalf("no entity: %v", err)
		}
		b, rerr := io.ReadAll(e.Body)
		return fmt.Sprintf("%q err=%v", b, rerr != nil)
	}
	if whole, small := body(bytes.NewReader(in)), body(&skNoEmptyReads{bytes.NewReader(in), 1}); whole != small {
		t.Errorf("read a byte at a time: %s; whole: %s", small, whole)
	}
}

// TestSoak_diagnosticOrder is what seed 1300007, iteration 214 found. The
// carrier of an embedded message is quoted-printable; the decoder's
// diagnostics and those of the embedded message's parse interleaved in an
// order that depended on how far the embedded header reader had read ahead
// into the decoder, so reading the input in small chunks reordered them. The
// decoder now reports a repair when its consumer has read up to it, and the
// carrier's body is read in whole blocks (chunk_invariance.go).
func TestSoak_diagnosticOrder(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "soak", "qp-embedded-diagnostic-order.eml"))
	if err != nil {
		t.Fatal(err)
	}
	order := func(src io.Reader) []string {
		s, e, _ := message.Parse(src, &message.ParseOptions{WalkEmbeddedMessages: true})
		e.Drain()
		var out []string
		for _, d := range s.Diagnostics() {
			out = append(out, fmt.Sprintf("%s %v", d.Code, d.Path))
		}
		return out
	}
	whole := order(bytes.NewReader(in))
	cfg := skConfig{chunkMax: 3}
	if got := order(cfg.reader(in, nil)); !slices.Equal(got, whole) {
		t.Errorf("in chunks of 3 the diagnostics come in another order:\n%q\nwhole:\n%q", got, whole)
	}
}
