package message

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Parser and decoder benchmarks (ROB-130-006), and the message shapes the
// complexity tests in complexity_test.go scale (ROB-060-013, ROB-120-002).
//
// Every input is built in memory from a fixed size, at most pfBenchMax bytes,
// and is parsed with the default ParseOptions: the budgets a deployment gets
// are part of what is measured. docs/robustness-notes/performance.md has the
// baseline numbers and how to read them.
//
//	go test -run '^$' -bench 'ParseDrain' -benchtime 200ms .

// pfBenchMax bounds every input these benchmarks and tests build.
const pfBenchMax = 8 << 20

const (
	pfBoundary = "=_emlkit_bench_0123456789abcdef"
	pfHead     = "From: Alice Example <alice@example.com>\r\n" +
		"To: Bob Example <bob@example.org>\r\n" +
		"Subject: benchmark message\r\n" +
		"Date: Wed, 11 Nov 2026 10:00:00 +0900\r\n" +
		"Message-ID: <bench.0001@example.com>\r\n" +
		"MIME-Version: 1.0\r\n"
)

// pfBoundary70 is a boundary of the greatest length RFC 2046 allows, whose
// first 69 bytes a near match shares.
var pfBoundary70 = strings.Repeat("a", 69) + "Z"

// A pfShape is one kind of message, built at a requested size.
type pfShape struct {
	name string
	// build returns a message of about n bytes. A shape whose size is a count
	// of something bounded by a default limit caps it there.
	build func(n int) []byte
	// bench is the size the benchmark builds. scale, when not zero, is the
	// smaller of the two sizes the complexity test compares, read whole; the
	// larger is four times that. slow excludes the shape from the small-read
	// variants of that test, which cost a call of every layer per byte.
	bench int
	scale int
	slow  bool
	// headerText makes a run decode every header field of the message as
	// text, as a consumer reading Subject or From does. parameters makes it
	// read the media type, the disposition and the filename of every entity,
	// as mail.Reader does.
	headerText bool
	parameters bool
	// What a run must find, each checked when set: entities in all, the
	// diagnostics codes, and at least delivered times the size of the message
	// in decoded body bytes.
	entities  func(msg []byte) int64
	codes     []Code
	delivered float64
	// boundary is the boundary of a near-match shape, whose multipart body
	// the complexity test also reads at the wire layer.
	boundary string
}

// pfRepeat returns unit repeated to at least n bytes.
func pfRepeat(unit string, n int) []byte {
	if n < len(unit) {
		n = len(unit)
	}
	return bytes.Repeat([]byte(unit), (n+len(unit)-1)/len(unit))
}

// pfOnePart frames body as the one part of a multipart/mixed message.
func pfOnePart(boundary, partHeader string, body []byte, closed bool) []byte {
	var b bytes.Buffer
	b.Grow(len(body) + 1024)
	b.WriteString(pfHead)
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\n%s\r\n", boundary, partHeader)
	b.Write(body)
	if closed {
		fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	}
	return b.Bytes()
}

const pfOpaque = "Content-Type: application/octet-stream\r\n"

// pfNear returns a shape whose one part holds lines that are near matches of
// the delimiter line.
func pfNear(name, boundary, unit string) pfShape {
	return pfShape{
		name: "near/" + name,
		build: func(n int) []byte {
			return pfOnePart(boundary, pfOpaque, pfRepeat(unit, n), true)
		},
		bench:     1 << 20,
		scale:     256 << 10,
		entities:  func([]byte) int64 { return 2 },
		delivered: 0.9,
		boundary:  boundary,
	}
}

// pfBase64 returns n bytes of base64 text in lines of 76 characters.
func pfBase64(n int) []byte {
	raw := make([]byte, n/78*57+57)
	for i := range raw {
		raw[i] = byte(i*131 + i>>8)
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	var b bytes.Buffer
	b.Grow(len(enc) + len(enc)/76*2 + 2)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	return b.Bytes()
}

// pfQP returns about n bytes of quoted-printable text: escaped UTF-8 and soft
// line breaks, as a body in a language outside ASCII is sent.
func pfQP(n int) []byte {
	const line = "=EC=95=88=EB=85=95=ED=95=98=EC=84=B8=EC=9A=94 hello =E2=82=AC 12=3D12 =\r\n" +
		"=ED=95=9C=EA=B8=80 text with trailing space \r\nplain line of ordinary text, nothing to decode here.\r\n"
	return pfRepeat(line, n)
}

// pfUULine returns raw, at most 45 bytes, as one uuencoded line.
func pfUULine(raw []byte) string {
	enc := func(v byte) byte {
		if v &= 63; v != 0 {
			return ' ' + v
		}
		return '`'
	}
	out := []byte{enc(byte(len(raw)))}
	for i := 0; i < len(raw); i += 3 {
		var q [3]byte
		copy(q[:], raw[i:])
		out = append(out, enc(q[0]>>2), enc(q[0]<<4|q[1]>>4), enc(q[1]<<2|q[2]>>6), enc(q[2]))
	}
	return string(out) + "\r\n"
}

// pfDeep returns a message of depth nested multiparts, the innermost holding
// one text part of about n bytes.
func pfDeep(depth, n int) []byte {
	var b bytes.Buffer
	b.Grow(n + depth*160)
	b.WriteString(pfHead)
	for d := 0; d < depth; d++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"lvl-%03d\"\r\n\r\n--lvl-%03d\r\n", d, d)
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.Write(pfRepeat("the innermost part of a deeply nested message.\r\n", n))
	for d := depth - 1; d >= 0; d-- {
		fmt.Fprintf(&b, "\r\n--lvl-%03d--\r\n", d)
	}
	return b.Bytes()
}

// pfInferNested returns a message of depth nested multiparts that declare no
// boundary and are never closed, the innermost holding one text part: about n
// bytes in all, within the default MaxPreambleBytes. Each level has to keep
// its whole body to find the boundary it is framed on.
func pfInferNested(depth, n int) []byte {
	n = min(n, 900<<10)
	var b bytes.Buffer
	b.Grow(n + depth*64)
	b.WriteString(pfHead)
	for d := 0; d < depth; d++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed\r\n\r\n--nest-%03d\r\n", d)
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.Write(pfRepeat("the innermost part of nested multiparts that declare no boundary.\r\n", n-b.Len()))
	return b.Bytes()
}

// pfWide returns a multipart of count small parts, each built by part.
func pfWide(count int, declare bool, part func(b *bytes.Buffer, i int)) []byte {
	var b bytes.Buffer
	b.Grow(count * 128)
	b.WriteString(pfHead)
	if declare {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", pfBoundary)
	} else {
		b.WriteString("Content-Type: multipart/mixed\r\n\r\n")
	}
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "--%s\r\n", pfBoundary)
		part(&b, i)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", pfBoundary)
	return b.Bytes()
}

// pfParts counts the parts of a message pfWide built.
func pfParts(msg []byte) int64 {
	return 1 + int64(bytes.Count(msg, []byte("--"+pfBoundary+"\r\n")))
}

// pfPartCount turns a size into a number of parts of about size bytes each,
// within the default MaxPartCount.
func pfPartCount(n, size int) int {
	return max(2, min(n/size, 9000))
}

func pfPlainPart(b *bytes.Buffer, i int) {
	fmt.Fprintf(b, "Content-Type: text/plain; charset=utf-8\r\n\r\npart number %06d of a wide message", i)
}

// pfParameterPart is a part whose header is all MIME parameters: RFC 2231
// continuations, an encoded word in a quoted string, and parameters to skip.
func pfParameterPart(b *bytes.Buffer, i int) {
	fmt.Fprintf(b, "Content-Type: application/pdf; x-unused-a=1; x-unused-b=\"two\";\r\n"+
		"\tname*0*=utf-8''%%ED%%95%%9C%%EA%%B8%%80; name*1*=%%20report%%20;\r\n"+
		"\tname*2=\"number %06d\"; name*3=.pdf\r\n"+
		"Content-Disposition: attachment; size=12345;\r\n"+
		"\tfilename=\"=?UTF-8?B?7ZWc6riAIHJlcG9ydA==?= %06d.pdf\";\r\n"+
		"\tcreation-date=\"Wed, 11 Nov 2026 10:00:00 +0900\"\r\n"+
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK", i, i)
}

// pfHeaderOnly returns a message whose header holds fields built by field
// until it is about n bytes, and a short body.
func pfHeaderOnly(n, maxFields int, field func(b *bytes.Buffer, i int)) []byte {
	var b bytes.Buffer
	b.Grow(n + 512)
	b.WriteString(pfHead)
	for i := 0; i < maxFields && b.Len() < n; i++ {
		field(&b, i)
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n")
	return b.Bytes()
}

// pfLongFields returns a message of about n bytes, within the default
// MaxHeaderBytes, whose header is a few fields as long as that allows, up to
// 56 KiB each, which the default MaxHeaderFieldBytes still admits.
func pfLongFields(n int, field func(b *bytes.Buffer, i, size int)) []byte {
	n = min(n, 900<<10)
	size := max(256, min(n/4, 56<<10))
	return pfHeaderOnly(n, 1000, func(b *bytes.Buffer, i int) { field(b, i, size) })
}

// pfEncodedWords is a Subject of adjacent and separated encoded words in both
// encodings, folded as a mail client folds it.
const pfEncodedWords = "Subject: =?UTF-8?B?7JWI64WV7ZWY7IS47JqUIOuwmOqwkeyKteuLiOuLpA==?=\r\n" +
	" =?UTF-8?B?7J6Q66OMIOyyqOu2gCDtjIzsnbw=?= plain words between\r\n" +
	" =?ISO-8859-1?Q?Andr=E9_Pirard_caf=E9?= =?utf-8?q?=E2=82=AC_100?=\r\n" +
	" =?UTF-8?B?4pyTIOKckyDinJM=?= (=?utf-8?Q?comment?=) =?us-ascii?Q?the_end?=\r\n"

// pfSmall is an ordinary short plain-text message.
const pfSmall = "Return-Path: <alice@example.com>\r\n" +
	"Received: from mail.example.com (mail.example.com [192.0.2.1])\r\n" +
	"\tby mx.example.org (Postfix) with ESMTPS id 4F2A91C0\r\n" +
	"\tfor <bob@example.org>; Wed, 11 Nov 2026 10:00:01 +0900 (KST)\r\n" +
	pfHead +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: 7bit\r\n" +
	"\r\n" +
	"Hi Bob,\r\n\r\nThe report is attached to the ticket. Let me know if the numbers\r\n" +
	"look right to you and I will send it on.\r\n\r\n-- \r\nAlice\r\n"

// pfTypical is the message most mail is: alternative text and HTML, and one
// attachment of about n bytes.
func pfTypical(n int) []byte {
	var b bytes.Buffer
	b.Grow(n + n/2 + 2048)
	b.WriteString(pfHead)
	b.WriteString("Content-Type: multipart/mixed; boundary=\"outer-b1\"\r\n\r\nThis is a multi-part message in MIME format.\r\n")
	b.WriteString("--outer-b1\r\nContent-Type: multipart/alternative; boundary=\"inner-b2\"\r\n\r\n")
	b.WriteString("--inner-b2\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.Write(pfQP(n / 8))
	b.WriteString("\r\n--inner-b2\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.WriteString("<html><body><p>")
	b.Write(pfQP(n / 4))
	b.WriteString("</p></body></html>\r\n--inner-b2--\r\n")
	b.WriteString("--outer-b1\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	b.Write(pfBase64(n))
	b.WriteString("--outer-b1--\r\n")
	return b.Bytes()
}

// pfInferred returns a multipart that declares no boundary, of parts of about
// 1 KiB, n bytes in all and within the default MaxPreambleBytes, which is what
// inference reads.
func pfInferred(n int) []byte {
	n = min(n, 900<<10)
	filler := string(pfRepeat("inferred boundary part body line\r\n", 960))
	return pfWide(max(2, n/1024), false, func(b *bytes.Buffer, i int) {
		fmt.Fprintf(b, "Content-Type: text/plain; charset=utf-8\r\n\r\n%s%06d", filler, i)
	})
}

// pfCandidates returns a multipart that declares no boundary and whose body
// is lines that each begin another boundary candidate, n bytes in all.
func pfCandidates(n int) []byte {
	n = min(n, 900<<10)
	var b bytes.Buffer
	b.Grow(n + 512)
	b.WriteString(pfHead)
	b.WriteString("Content-Type: multipart/mixed\r\n\r\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "--candidate-%07d\r\nContent-Type: text/plain\r\n\r\nx\r\n", i)
	}
	return b.Bytes()
}

func pfTextQP(body []byte) []byte {
	return append([]byte(pfHead+"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"), body...)
}

func pfOne([]byte) int64 { return 1 }

// pfShapes is every shape, in the order the notes list them.
var pfShapes = []pfShape{
	{
		name:      "small/plain",
		build:     func(int) []byte { return []byte(pfSmall) },
		entities:  pfOne,
		delivered: 0.2,
	},
	{
		name:      "typical/alternative+attachment",
		build:     pfTypical,
		bench:     96 << 10,
		scale:     64 << 10,
		entities:  func([]byte) int64 { return 5 },
		delivered: 0.5,
	},
	{
		name: "attachment/base64",
		build: func(n int) []byte {
			return pfOnePart(pfBoundary, "Content-Type: application/zip\r\nContent-Transfer-Encoding: base64\r\n", pfBase64(n), true)
		},
		bench:     4 << 20,
		scale:     256 << 10,
		entities:  func([]byte) int64 { return 2 },
		delivered: 0.7,
	},
	{
		// Junk, URL-safe characters, padding inside the data and an indented
		// line: every repair the tolerant decoder makes.
		name: "attachment/base64-damaged",
		build: func(n int) []byte {
			unit := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo!@#YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXo-_w==\r\n" +
				"  MDEyMzQ1Njc4OQ==QUJD=\r\n"
			return pfOnePart(pfBoundary, "Content-Type: application/zip\r\nContent-Transfer-Encoding: base64\r\n", pfRepeat(unit, n), true)
		},
		bench:     1 << 20,
		scale:     256 << 10,
		entities:  func([]byte) int64 { return 2 },
		codes:     []Code{CodeCTEBase64Junk, CodeCTEBase64DataAfterPadding},
		delivered: 0.5,
	},
	{
		name:      "text/quoted-printable",
		build:     func(n int) []byte { return pfTextQP(pfQP(n)) },
		bench:     1 << 20,
		scale:     256 << 10,
		entities:  pfOne,
		delivered: 0.3,
	},
	{
		name: "text/quoted-printable-damaged",
		build: func(n int) []byte {
			return pfTextQP(pfRepeat("=ZZ bad escape =\t \r\nsoft break with white space = x\r\ncontrol \x01 byte =4\r\n=\rbare CR soft break\r\n", n))
		},
		bench:     1 << 20,
		scale:     256 << 10,
		entities:  pfOne,
		codes:     []Code{CodeCTEQPInvalidHex, CodeCTEQPInvalidSoftBreak},
		delivered: 0.5,
	},
	{
		name: "text/uuencode",
		build: func(n int) []byte {
			body := append([]byte("begin 644 data.bin\r\n"), pfRepeat(pfUULine([]byte("The quick brown fox jumps over the lazy dog. ")), n)...)
			body = append(body, "`\r\nend\r\n"...)
			return append([]byte(pfHead+"Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: x-uuencode\r\n\r\n"), body...)
		},
		bench:     1 << 20,
		scale:     256 << 10,
		entities:  pfOne,
		delivered: 0.7,
	},
	{
		// Every byte of the innermost part passes through one framing reader
		// per level: the cost is the size times the depth, and the depth is
		// what MaxMIMEDepth bounds.
		name:      "mime/deep-100",
		build:     func(n int) []byte { return pfDeep(100, n) },
		bench:     1 << 20,
		scale:     128 << 10,
		slow:      true,
		entities:  func([]byte) int64 { return 101 },
		delivered: 0.8,
	},
	{
		name:      "mime/deep-10",
		build:     func(n int) []byte { return pfDeep(10, n) },
		bench:     1 << 20,
		scale:     128 << 10,
		entities:  func([]byte) int64 { return 11 },
		delivered: 0.5,
	},
	{
		name:     "mime/wide",
		build:    func(n int) []byte { return pfWide(pfPartCount(n, 100), true, pfPlainPart) },
		bench:    900 << 10,
		scale:    200 << 10,
		entities: pfParts,
	},
	{
		name:       "mime/wide-parameters",
		build:      func(n int) []byte { return pfWide(pfPartCount(n, 420), true, pfParameterPart) },
		bench:      900 << 10,
		scale:      200 << 10,
		parameters: true,
		entities:   pfParts,
	},
	{
		// Parts with no empty line after their header and a damaged base64
		// body: header recovery hands the body's first line back to the
		// framing reader, and every part records diagnostics.
		name: "mime/wide-damaged",
		build: func(n int) []byte {
			return pfWide(pfPartCount(n, 135), true, func(b *bytes.Buffer, i int) {
				fmt.Fprintf(b, "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n!!part %06d has no separator!!\r\nQUJD=RA", i)
			})
		},
		bench:    900 << 10,
		scale:    200 << 10,
		entities: pfParts,
		codes:    []Code{CodeHeaderMissingBodySeparator, CodeCTEBase64Junk, CodeLimitDiagnosticCount},
	},
	{
		name: "mime/unclosed",
		build: func(n int) []byte {
			return pfOnePart(pfBoundary, pfOpaque, pfRepeat("a line of the last part of a multipart that is never closed\r\n", n), false)
		},
		bench:     1 << 20,
		scale:     256 << 10,
		entities:  func([]byte) int64 { return 2 },
		codes:     []Code{CodeMultipartCloseNotFound},
		delivered: 0.9,
	},
	{
		name:      "mime/boundary-inferred",
		build:     pfInferred,
		bench:     900 << 10,
		scale:     200 << 10,
		entities:  pfParts,
		codes:     []Code{CodeMultipartBoundaryInferred},
		delivered: 0.8,
	},
	{
		name:      "mime/boundary-candidates",
		build:     pfCandidates,
		bench:     900 << 10,
		scale:     200 << 10,
		entities:  func([]byte) int64 { return 2 },
		codes:     []Code{CodeMultipartBoundaryInferred, CodeMultipartBoundaryAmbiguous},
		delivered: 0.9,
	},
	{
		// Multiparts that declare no boundary, nested to the depth the default
		// budget admits. Each level that infers its boundary keeps and reads
		// its whole body again, which multipart_heal_nest.go bounds: the
		// eleventh level is returned unframed.
		name:      "mime/boundary-inferred-nested-100",
		build:     func(n int) []byte { return pfInferNested(100, n) },
		bench:     900 << 10,
		scale:     64 << 10,
		slow:      true,
		entities:  func([]byte) int64 { return pfInferNestMax + 3 },
		codes:     []Code{CodeMultipartBoundaryInferred, CodeMultipartCloseNotFound, CodeMultipartStartNotFound},
		delivered: 0.7,
	},
	pfNear("dashes", pfBoundary, "--\r\n"),
	pfNear("prefix", pfBoundary, "--"+pfBoundary[:8]+"\r\n"),
	pfNear("minus-one", pfBoundary, "--"+pfBoundary[:len(pfBoundary)-1]+"\r\n"),
	pfNear("70-shared-prefix", pfBoundary70, "--"+pfBoundary70[:69]+"Y\r\n"),
	pfNear("trailing-junk", pfBoundary, "--"+pfBoundary+" junk\r\n--"+pfBoundary+"--junk\r\n--"+pfBoundary+"-\r\n"),
	pfNear("padded", pfBoundary, "--"+pfBoundary+strings.Repeat(" \t", 150)+"x\r\n"),
	pfNear("padded-past-buffer", pfBoundary, "--"+pfBoundary+strings.Repeat(" ", 6000)+"x\r\n"),
	pfNear("cr-only", pfBoundary, "--"+pfBoundary[:len(pfBoundary)-1]+"\r"),
	pfNear("bare-lf", pfBoundary, "--"+pfBoundary[:len(pfBoundary)-1]+"\n"),
	pfNear("one-long-line", pfBoundary, "--"+pfBoundary),
	pfNear("dash-run", strings.Repeat("-", 68)+"x", strings.Repeat("-", 70)),
	pfNear("dash-run-match", strings.Repeat("-", 70), strings.Repeat("-", 200)+"\r\n"),
	{
		name: "header/many-fields",
		build: func(n int) []byte {
			return pfHeaderOnly(n, 1000, func(b *bytes.Buffer, i int) {
				fmt.Fprintf(b, "X-Bench-Field-%04d: the value of an ordinary header field, number %d\r\n", i, i)
			})
		},
		bench:    64 << 10,
		scale:    16 << 10,
		entities: pfOne,
	},
	{
		name: "header/long-folded",
		build: func(n int) []byte {
			return pfLongFields(n, func(b *bytes.Buffer, i, size int) {
				fmt.Fprintf(b, "X-Bench-Folded-%04d: first line\r\n", i)
				b.Write(pfRepeat("\tcontinuation line of a very long folded field, seventy bytes or so\r\n", size))
			})
		},
		bench:    900 << 10,
		scale:    200 << 10,
		entities: pfOne,
	},
	{
		name: "header/long-lines",
		build: func(n int) []byte {
			return pfLongFields(n, func(b *bytes.Buffer, i, size int) {
				fmt.Fprintf(b, "X-Bench-Line-%04d: ", i)
				b.Write(pfRepeat("one line with no fold in it ", size))
				b.WriteString("\r\n")
			})
		},
		bench:    900 << 10,
		scale:    200 << 10,
		entities: pfOne,
	},
	{
		// Damaged lines between good fields: each one makes header recovery
		// look ahead for a field that shows the block goes on.
		name: "header/damaged-lines",
		build: func(n int) []byte {
			return pfHeaderOnly(n, 120, func(b *bytes.Buffer, i int) {
				for j := 0; j < 7; j++ {
					fmt.Fprintf(b, "a damaged header line %d %d with no colon in it\r\n", i, j)
				}
				fmt.Fprintf(b, "X-Bench-Good-%04d: a field that shows the block goes on\r\n", i)
			})
		},
		bench:    48 << 10,
		scale:    12 << 10,
		entities: pfOne,
		codes:    []Code{CodeHeaderMissingColon},
	},
	{
		name: "header/rfc2047",
		build: func(n int) []byte {
			return pfHeaderOnly(n, 1000, func(b *bytes.Buffer, i int) {
				b.WriteString(pfEncodedWords)
			})
		},
		bench:      128 << 10,
		scale:      32 << 10,
		headerText: true,
		entities:   pfOne,
	},
	{
		// Fields that are nothing but candidates for encoded words, most of
		// them never closed.
		name: "header/rfc2047-unclosed",
		build: func(n int) []byte {
			return pfLongFields(n, func(b *bytes.Buffer, i, size int) {
				b.WriteString("Subject: ")
				b.Write(pfRepeat("=?utf-8?q?open =?utf-8?b?QUJD =?x?= =?utf-8?q?", size))
				b.WriteString("\r\n")
			})
		},
		bench:      900 << 10,
		scale:      200 << 10,
		headerText: true,
		entities:   pfOne,
	},
}

// pfRun parses msg from r and reads all of it: Parse, what the shape asks to
// be read of the headers, and Entity.Drain.
func pfRun(tb testing.TB, sh *pfShape, r io.Reader) *ParseSession {
	s, e, err := Parse(r, nil)
	if e == nil {
		tb.Fatalf("%s: Parse returned no entity: %v", sh.name, err)
	}
	if sh.headerText {
		for f := e.Header.Fields(); f.Next(); {
			DecodeHeaderText(f.Value())
		}
	}
	if sh.parameters {
		// An entity is read once, so this walk is the drain too.
		werr := e.Walk(func(_ []int, p *Entity, _ error) error {
			p.EffectiveContentType()
			p.EffectiveContentDisposition()
			p.FilenameCandidates()
			if p.MultipartReader() == nil && p.Body != nil {
				io.Copy(io.Discard, p.Body)
			}
			return nil
		})
		if werr != nil {
			tb.Fatalf("%s: Walk: %v", sh.name, werr)
		}
	}
	if derr := e.Drain(); derr != nil {
		tb.Fatalf("%s: Drain: %v", sh.name, derr)
	}
	return s
}

// BenchmarkParseDrain parses and drains each shape at its benchmark size. It
// reports MB/s of input and allocations per message.
func BenchmarkParseDrain(b *testing.B) {
	for i := range pfShapes {
		sh := &pfShapes[i]
		b.Run(sh.name, func(b *testing.B) {
			msg := sh.build(sh.bench)
			if len(msg) > pfBenchMax {
				b.Fatalf("input of %d bytes is over the cap of %d", len(msg), pfBenchMax)
			}
			r := bytes.NewReader(msg)
			b.SetBytes(int64(len(msg)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Reset(msg)
				pfRun(b, sh, r)
			}
		})
	}
}

// BenchmarkDecodeHeaderText decodes one header value of many encoded words.
func BenchmarkDecodeHeaderText(b *testing.B) {
	value := strings.TrimPrefix(strings.Repeat(pfEncodedWords, 8), "Subject: ")
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		DecodeHeaderText(value)
	}
}

// pfDelivered returns how many decoded body bytes the leaves of msg deliver.
func pfDelivered(t *testing.T, msg []byte) int64 {
	_, e, err := Parse(bytes.NewReader(msg), nil)
	if e == nil {
		t.Fatalf("Parse returned no entity: %v", err)
	}
	var n int64
	werr := e.Walk(func(_ []int, p *Entity, _ error) error {
		if p.MultipartReader() == nil && p.Body != nil {
			m, _ := io.Copy(io.Discard, p.Body)
			n += m
		}
		return nil
	})
	if werr != nil {
		t.Fatalf("Walk: %v", werr)
	}
	return n
}

// TestBenchmarkShapes checks that each shape is the message it is meant to
// be: a benchmark of an input the parser reads some other way measures
// nothing.
func TestBenchmarkShapes(t *testing.T) {
	for i := range pfShapes {
		sh := &pfShapes[i]
		t.Run(sh.name, func(t *testing.T) {
			size := sh.scale
			if size == 0 {
				size = sh.bench
			}
			msg := sh.build(size)
			if len(msg) > pfBenchMax {
				t.Fatalf("input of %d bytes is over the cap of %d", len(msg), pfBenchMax)
			}
			s := pfRun(t, sh, bytes.NewReader(msg))
			c := s.Completion()
			if c.Stop != nil {
				t.Fatalf("the parse was stopped: %v", c.Stop)
			}
			if c.BodiesRead != c.Bodies || c.MultipartsRead != c.Multiparts {
				t.Errorf("not read whole: %+v", c)
			}
			if sh.entities != nil {
				if want := sh.entities(msg); c.Entities != want {
					t.Errorf("found %d entities, want %d", c.Entities, want)
				}
			}
			if got, want := pfDelivered(t, msg), int64(sh.delivered*float64(len(msg))); got < want {
				t.Errorf("the bodies deliver %d bytes of a message of %d, want at least %d", got, len(msg), want)
			}
			seen := make(map[Code]bool)
			for _, d := range s.Diagnostics() {
				seen[d.Code] = true
			}
			for _, code := range sh.codes {
				if !seen[code] {
					t.Errorf("no %s diagnostic; recorded %v", code, pfCodes(seen))
				}
			}
			if len(sh.codes) == 0 {
				for code := range seen {
					if code.Group() == "limit" {
						t.Errorf("unexpected %s diagnostic", code)
					}
				}
			}
		})
	}
}

func pfCodes(seen map[Code]bool) []Code {
	var codes []Code
	for code := range seen {
		codes = append(codes, code)
	}
	return codes
}
