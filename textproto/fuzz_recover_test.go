package textproto

import (
	"bufio"
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// FuzzReadHeaderRecover is the native fuzz target of the recovering header
// reader (ROB-130-001); docs/robustness-notes/fuzzing.md lists its invariants
// (ROB-130-002). Every source is a finite in-memory input of at most 16 KiB.

// fzRecoverOutcome is everything one ReadHeaderRecover reports, with what was
// left in the reader after it.
type fzRecoverOutcome struct {
	fields     []wantField
	ends       LineEndings
	bom        ByteOrderMark
	prefix     prefixOutcome
	records    []recordOutcome
	joined     int
	joinedAt   int64
	sepMissing bool
	sepAt      int64
	wsLine     bool
	unread     string
	rest       string
	err        string
}

func fzReadRecover(t *testing.T, br *bufio.Reader, lim HeaderLimits) (Header, fzRecoverOutcome) {
	t.Helper()
	h, unread, err := ReadHeaderRecover(br, lim)
	o := fzRecoverOutcome{
		fields: fieldsOf(t, h), ends: h.LineEndings(), bom: h.ByteOrderMark(), prefix: prefixOutcomeOf(&h),
		records: recordsOf(&h), unread: string(unread),
	}
	o.joined, o.joinedAt = h.UnindentedContinuations()
	o.sepMissing, o.sepAt, o.wsLine = h.BodySeparatorMissing()
	if err != nil {
		o.err = err.Error()
	}
	rest, rerr := io.ReadAll(br)
	if rerr != nil {
		t.Fatalf("reading what follows the block = %v", rerr)
	}
	o.rest = string(rest)
	return h, o
}

// fzGenerousLimits are what mail's tree view reads a header under, and
// fzTightLimits small enough that inputs the fuzzer finds reach every budget.
var (
	fzGenerousLimits = HeaderLimits{MaxBytes: 1 << 20, MaxCount: 4096}
	fzTightLimits    = HeaderLimits{MaxBytes: 1024, MaxFieldBytes: 256, MaxCount: 16, MaxLineBytes: 128}
)

// FuzzReadHeaderRecover reads an arbitrary block with header recovery and
// checks that:
//
//   - it ends, under generous and under tight budgets, and gives the same
//     outcome — fields, line endings, mark, prefix, records, repairs, unread
//     bytes, what is left in the reader, error — whether the input arrives
//     whole, a byte at a time, seven bytes at a time or with io.EOF on its
//     last bytes (the reader's buffer is the default 4096 bytes throughout:
//     the decision may depend on a smaller one, as recover.go says);
//   - the bytes handed back as unread are exactly the last bytes it consumed,
//     and are handed back only when the block ended before a line that began
//     the body, at the place it reports;
//   - each orphan record is the input's own bytes at its location, inside the
//     block;
//   - written back, a complete CRLF block is unchanged byte for byte, and one
//     that ended at a body line is that block and the empty line it lacked:
//     the documented contract, which leaves out blocks with other line
//     endings (stored as CRLF, ROB-020-007) and a line with an empty name
//     (dropped, ROB-020-006);
//   - read back, the written header has the same fields and writes the same
//     bytes again.
func FuzzReadHeaderRecover(f *testing.F) {
	for _, s := range []string{
		"Subject: s\r\n\r\nbody",
		"MIME-Version: 1.0\r\nSubject: hi\r\ndamaged line\r\nContent-Type: text/plain;\r\ncharset=utf-8\r\nX-Last: 1\r\n\r\nbody\r\n",
		"Content-Type: multipart/mixed;\r\nboundary=\"B\"\r\n\r\n--B\r\n",
		"To: a@example.com,\r\nb@example.com\r\nSubject: x\r\n\r\n",
		"Received: from a\r\nby b; Thu, 1 Jan 2026 00:00:00 +0000\r\n\r\n",
		"Subject: s\r\nHello Bob,\r\nbody without a separator\r\n",
		"Subject: s\r\n   \r\nbody after a white space line\r\n",
		"Subject: 제목 한글\r\nFrom: 山田 <yamada@example.jp>\r\n本文がここから\r\n",
		"Subject: =?GB2312?B?1tDOxA==?=\r\nBad Name: y\r\nX-A: 1\r\n\r\n中文正文",
		": empty\r\nA: 1\r\n\r\n",
		"\xef\xbb\xbfSubject: bom\r\n\r\n",
		" lead\r\n\tmore\r\nA: 1\r\n\r\n",
		"A: 1\nbroken\n o\nB: 2\n\nbody",
		"broken\r o\rB: 2\r\rbody",
		"A: 1\r\nX\x00\xff: v\r\n \x01\r\n\r\n",
		"Subject: s\r\n" + strings.Repeat("no colon here\r\n", 70) + "X-A: 1\r\n\r\n",
		"From x Thu Jan  1 00:00:00 2026\r\nSubject: s\r\n\r\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16<<10 {
			return
		}
		s := string(data)
		for _, lim := range []HeaderLimits{fzGenerousLimits, fzTightLimits} {
			h, want := fzReadRecover(t, bufio.NewReader(strings.NewReader(s)), lim)
			for name, newReader := range map[string]func() *bufio.Reader{
				"bytewise":      func() *bufio.Reader { return bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(s)), 4096) },
				"chunks of 7":   func() *bufio.Reader { return bufio.NewReader(&chunkReader{s: s, n: 7}) },
				"data with eof": func() *bufio.Reader { return bufio.NewReader(iotest.DataErrReader(strings.NewReader(s))) },
			} {
				if _, got := fzReadRecover(t, newReader(), lim); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: the delivery changed the outcome\n got %+v\nwant %+v", name, got, want)
				}
			}
			if lim == fzGenerousLimits {
				fzCheckRecovered(t, data, h, want)
			}
		}
	})
}

func fzCheckRecovered(t *testing.T, data []byte, h Header, o fzRecoverOutcome) {
	t.Helper()
	consumed := len(data) - len(o.rest)
	end := consumed - len(o.unread)
	if end < 0 || string(data[end:consumed]) != o.unread {
		t.Fatalf("unread %q is not the last of the %d bytes consumed", o.unread, consumed)
	}
	if o.unread != "" && (!o.sepMissing || o.sepAt != int64(end)) {
		t.Fatalf("unread %q at %d, but the block reports separator missing %v at %d", o.unread, end, o.sepMissing, o.sepAt)
	}
	for _, r := range o.records {
		if r.start < 0 || r.end > int64(end) || r.start > r.end || string(data[r.start:r.end]) != r.bytes {
			t.Fatalf("record %+v is not the input's bytes inside the block of %d bytes", r, end)
		}
	}
	if o.err != "" {
		return
	}

	var w bytes.Buffer
	if err := WriteHeader(&w, h); err != nil {
		if !o.bom.wide() {
			t.Fatalf("WriteHeader() = %v", err)
		}
		return
	}
	block := data[:end]
	if fzCRLFOnly(block) && !fzEmptyNameLine(block) {
		want := ""
		switch {
		case o.sepMissing:
			want = string(block) + "\r\n"
		case bytes.HasSuffix(block, []byte("\r\n\r\n")) || string(block) == "\r\n":
			want = string(block)
		default:
			want = w.String() // ended by the end of the input: not pinned here
		}
		if w.String() != want {
			t.Fatalf("written back %q\nwant         %q", w.String(), want)
		}
	}

	if w.Len() >= recoverWindow {
		// The look past a damaged line spans one buffer; a longer header,
		// written with other line endings, may be decided otherwise.
		return
	}
	h2, again := fzReadRecover(t, bufio.NewReader(bytes.NewReader(w.Bytes())), fzGenerousLimits)
	if again.err != "" || !sameFields(again.fields, o.fields) {
		t.Fatalf("read back, %q has fields %+v (%s), want %+v", w.String(), again.fields, again.err, o.fields)
	}
	var w2 bytes.Buffer
	if err := WriteHeader(&w2, h2); err != nil || w2.String() != w.String() {
		t.Fatalf("written again %q (%v), want %q", w2.String(), err, w.String())
	}
}

// fzCRLFOnly reports whether every line ending of b is CRLF and b has no CR or
// LF otherwise.
func fzCRLFOnly(b []byte) bool {
	for i, c := range b {
		switch c {
		case '\r':
			if i+1 >= len(b) || b[i+1] != '\n' {
				return false
			}
		case '\n':
			if i == 0 || b[i-1] != '\r' {
				return false
			}
		}
	}
	return true
}

// fzEmptyNameLine reports whether a line of b begins with a colon: a field
// with an empty name, which the reader skips.
func fzEmptyNameLine(b []byte) bool {
	for line := range strings.SplitSeq(string(b), "\r\n") {
		if strings.HasPrefix(line, ":") {
			return true
		}
	}
	return false
}
