package textproto

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Wire-layer benchmarks (ROB-130-006): the multipart reader's delimiter scan
// over ordinary bodies and over bodies of lines that nearly match the
// delimiter, and the header reader over long and damaged blocks. The inputs
// are built in memory, 1 MiB each. The message package's BenchmarkParseDrain
// measures the same shapes through Parse, and its complexity tests hold both
// layers to linear cost; docs/robustness-notes/performance.md has the numbers.
//
//	go test -run '^$' -bench 'MultipartScan|ReadHeaderShapes' -benchtime 200ms ./textproto/

const (
	pfScanSize     = 1 << 20
	pfScanBoundary = "=_emlkit_bench_0123456789abcdef"
)

// pfScanRepeat returns unit repeated to at least n bytes.
func pfScanRepeat(unit string, n int) []byte {
	return bytes.Repeat([]byte(unit), (n+len(unit)-1)/len(unit))
}

// pfScanBody frames units of content as the one part of a multipart body.
func pfScanBody(boundary, unit string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "--%s\r\nContent-Type: application/octet-stream\r\n\r\n", boundary)
	b.Write(pfScanRepeat(unit, pfScanSize))
	fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	return b.Bytes()
}

// pfScanParts frames small parts, pfScanSize bytes of them in all.
func pfScanParts() []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < pfScanSize; i++ {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain\r\n\r\npart number %06d of a wide body\r\n", pfScanBoundary, i)
	}
	fmt.Fprintf(&b, "--%s--\r\n", pfScanBoundary)
	return b.Bytes()
}

var pfScanBodies = []struct {
	name     string
	boundary string
	body     func() []byte
}{
	{"text", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, "An ordinary line of text in the body of a part, about seventy bytes.\r\n")
	}},
	{"base64", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo+", 2)+"YWJj\r\n")
	}},
	{"many-parts", pfScanBoundary, pfScanParts},
	{"near/dashes", pfScanBoundary, func() []byte { return pfScanBody(pfScanBoundary, "--\r\n") }},
	{"near/minus-one", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, "--"+pfScanBoundary[:len(pfScanBoundary)-1]+"\r\n")
	}},
	{"near/70-shared-prefix", strings.Repeat("a", 69) + "Z", func() []byte {
		return pfScanBody(strings.Repeat("a", 69)+"Z", "--"+strings.Repeat("a", 69)+"Y\r\n")
	}},
	{"near/trailing-junk", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, "--"+pfScanBoundary+" junk\r\n--"+pfScanBoundary+"--junk\r\n")
	}},
	{"near/padded", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, "--"+pfScanBoundary+strings.Repeat(" \t", 150)+"x\r\n")
	}},
	{"near/cr-only", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, "--"+pfScanBoundary[:len(pfScanBoundary)-1]+"\r")
	}},
	{"near/bare-lf", pfScanBoundary, func() []byte {
		return pfScanBody(pfScanBoundary, "--"+pfScanBoundary[:len(pfScanBoundary)-1]+"\n")
	}},
	{"near/one-long-line", pfScanBoundary, func() []byte { return pfScanBody(pfScanBoundary, "--"+pfScanBoundary) }},
	{"near/dash-run-match", strings.Repeat("-", 70), func() []byte {
		return pfScanBody(strings.Repeat("-", 70), strings.Repeat("-", 200)+"\r\n")
	}},
	{"preamble/near-minus-one", pfScanBoundary, func() []byte {
		// No delimiter line before the lines: they are framing lines, kept as
		// the preamble.
		pre := pfScanRepeat("--"+pfScanBoundary[:len(pfScanBoundary)-1]+"\r\n", pfScanSize-4096)
		return append(pre, fmt.Sprintf("--%s\r\n\r\nx\r\n--%s--\r\n", pfScanBoundary, pfScanBoundary)...)
	}},
}

// BenchmarkMultipartScan reads every part of a multipart body to its end, on
// the terms Parse reads one: recovery on, and a framing line budget of
// 64 KiB.
func BenchmarkMultipartScan(b *testing.B) {
	limits := HeaderLimits{Recover: true, MaxLineBytes: 64 << 10}
	buf := make([]byte, 32<<10)
	for _, c := range pfScanBodies {
		b.Run(c.name, func(b *testing.B) {
			body := c.body()
			r := bytes.NewReader(body)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Reset(body)
				mr := NewMultipartReader(r, c.boundary)
				mr.SetHeaderLimits(limits)
				var read int64
				for {
					p, err := mr.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
					for {
						n, err := p.Read(buf)
						read += int64(n)
						if err != nil {
							break
						}
					}
				}
				if read < int64(len(body))/4 && !strings.HasPrefix(c.name, "preamble/") {
					b.Fatalf("the parts delivered %d bytes of a body of %d", read, len(body))
				}
			}
		})
	}
}

var pfScanHeaders = []struct {
	name    string
	recover bool
	block   func() []byte
}{
	{"many-fields", false, func() []byte {
		return append(pfScanRepeat("X-Field: the value of an ordinary header field\r\n", pfScanSize), "\r\n"...)
	}},
	{"long-folded", false, func() []byte {
		return append(append([]byte("X-Folded: first\r\n"), pfScanRepeat("\tcontinuation line of a very long folded field\r\n", pfScanSize)...), "\r\n"...)
	}},
	{"long-line", false, func() []byte {
		return append(append([]byte("X-Long: "), pfScanRepeat("one line with no fold in it ", pfScanSize)...), "\r\n\r\n"...)
	}},
	{"cr-inside-value", false, func() []byte {
		return append(append([]byte("X-Stray: "), pfScanRepeat("stray\rcarriage returns in one long value ", pfScanSize)...), "\r\n\r\n"...)
	}},
	{"damaged-lines", true, func() []byte {
		unit := strings.Repeat("a damaged header line with no colon in it\r\n", 7) + "X-Good: the block goes on\r\n"
		return append(pfScanRepeat(unit, pfScanSize), "\r\n"...)
	}},
	{"unindented-continuations", true, func() []byte {
		unit := "Content-Type: text/plain;\r\ncharset=utf-8;\r\nname=value\r\nTo: a@example.com,\r\nb@example.com,\r\nc@example.com\r\n"
		return append(pfScanRepeat(unit, pfScanSize), "\r\n"...)
	}},
}

// BenchmarkReadHeaderShapes reads one header block of 1 MiB, with no budget,
// through a reader buffer of 4096 bytes.
func BenchmarkReadHeaderShapes(b *testing.B) {
	for _, c := range pfScanHeaders {
		b.Run(c.name, func(b *testing.B) {
			block := c.block()
			r := bytes.NewReader(block)
			br := bufio.NewReaderSize(r, 4096)
			b.SetBytes(int64(len(block)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Reset(block)
				br.Reset(r)
				var err error
				if c.recover {
					_, _, err = ReadHeaderRecover(br, HeaderLimits{})
				} else {
					_, err = ReadHeader(br)
				}
				if err != nil {
					b.Fatal(err)
				}
				if br.Buffered() != 0 || r.Len() != 0 {
					b.Fatalf("%d bytes of the block were left unread", br.Buffered()+r.Len())
				}
			}
		})
	}
}
