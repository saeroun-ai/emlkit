package mail

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
)

// Preamble and epilogue in the structure view (ROB-060-001; corpus findings
// list-footer-after-close and nested-reused-boundary-deep).
//
// RFC 2046 says the bytes before a multipart's first delimiter line and after
// its closing one are to be ignored, and no part is read from them. They are
// where a mailing list appends its footer, where a sender hides text from
// every reader that goes by the parts, and where the parts of a multipart end
// up that a reused boundary closed too early. So ReadTree keeps both on the
// multipart's node, and records one multipart.trailing_junk note, of
// SeverityInfo, in Tree.Diagnostics for each epilogue that holds more than
// white space: how many bytes it holds, and whether they read as further
// delimiter lines of the multipart's own boundary, a header block, text, or
// binary data. The note carries no byte of the epilogue.
//
// The epilogue is read when the multipart's last part has been read, through
// message.MultipartEpilogue: at most Limits.MaxEpilogueBytes of it, which
// limit.epilogue_bytes reports when it was longer. For the message's own body
// under ProfileForensic that read goes through
// ParseSession.TrailingInputReader, whose snapshot, evidence and
// input.trailing_messages note account for the bytes, and which does not
// deliver them again. An unclosed multipart has no epilogue.

// obEpilogueAction is what ReadTree did with an epilogue that holds more than
// white space.
const obEpilogueAction = "kept the bytes as the node's Epilogue; no part was read from them"

// obMultipartEnds sets the preamble and epilogue of n, the node of the
// multipart mr has iterated to its end, and returns the parse's stop if
// reading the epilogue ran into one.
func (b *treeBuilder) obMultipartEnds(mr message.MultipartReader, n *Node) error {
	n.Preamble, n.PreambleTruncated, _ = message.MultipartPreamble(mr)
	n.Epilogue, n.EpilogueTruncated, _ = message.MultipartEpilogue(mr)
	return b.root.ParseStop()
}

// obTrailing records the multipart.trailing_junk note of every node under root
// whose epilogue holds more than white space, in document order.
func (b *treeBuilder) obTrailing(root *Node, t *Tree) {
	root.walk(func(n *Node) bool {
		if !obBlank(n.Epilogue) {
			size := fmt.Sprintf("%d bytes", len(n.Epilogue))
			if n.EpilogueTruncated {
				size = "more than " + size
			}
			t.Diagnostics = append(t.Diagnostics, message.Diagnostic{
				Code:     message.CodeMultipartTrailingJunk,
				Severity: message.SeverityInfo,
				Message: fmt.Sprintf("multipart epilogue, after the closing delimiter line, holds %s that are not only white space; they read as %s",
					size, obEpilogueKind(n.Epilogue, n.Params["boundary"])),
				Path:   append([]int(nil), n.Path...),
				Action: obEpilogueAction,
			})
		}
		return true
	})
}

// obBlank reports whether b holds nothing but spaces, tabs and line endings.
func obBlank(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

// obEpilogueKind says what an epilogue that is not blank reads as: delimiter
// lines of boundary, the multipart's own (parts that a closing delimiter cut
// off), a header block (a message appended after this one), text (a footer)
// or binary data.
func obEpilogueKind(b []byte, boundary string) string {
	dash := []byte("--" + boundary)
	first := true
	header := false
	for rest := b; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimRight(line, " \t\r")
		if boundary != "" && bytes.HasPrefix(line, dash) {
			if tail := line[len(dash):]; len(tail) == 0 || string(tail) == "--" {
				return "further delimiter lines of the multipart's boundary"
			}
		}
		if first && len(line) > 0 {
			first = false
			header = obFieldLine(line) || bytes.HasPrefix(line, []byte("From "))
		}
	}
	switch {
	case header:
		return "a header block"
	case obText(b):
		return "text"
	}
	return "binary data"
}

// obFieldLine reports whether line begins with a header field name and its
// colon.
func obFieldLine(line []byte) bool {
	i := bytes.IndexByte(line, ':')
	if i <= 0 || i > 76 {
		return false
	}
	for _, c := range line[:i] {
		if c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// obText reports whether b is UTF-8 with no control bytes but white space.
func obText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c < ' ' && c != '\t' && c != '\r' && c != '\n' && c != '\f' || c == 0x7f {
			return false
		}
	}
	return true
}
