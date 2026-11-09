package message

import (
	"fmt"
	"strings"
	"testing"
)

// A nested multipart's last delimiter is decided by the end of the outer part
// that holds it: the line ending after an inner closing delimiter is the
// outer delimiter's, and an inner delimiter line cut short by the outer
// delimiter is only one once the outer part has ended. These pin that a
// cancellation arriving while the outer delimiter is being decided does not
// change where the inner body ends: every inner body a cancelled parse
// delivers is, as for any cut, a prefix of what Parse delivers from the same
// input bytes followed by an ordinary failure (ROB-010-002, with ROB-001-006's
// sweep reference).

func TestParseContext_cancelNestedScopeEnd(t *testing.T) {
	const head = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n" +
		"--C\r\nContent-Type: text/plain\r\n\r\nnested\r\n"
	const tail = "\r\n--B\r\nContent-Type: text/plain\r\n\r\nlast\r\n--B--\r\n"
	for _, f := range []sweepFixture{
		{name: "inner close at the outer part's end", text: head + "--C--" + tail},
		{name: "inner close with white space at the outer part's end", text: head + "--C-- \t" + tail},
		{name: "inner delimiter cut by the outer part's end", text: head + "--C  " + tail},
	} {
		checkNestedCuts(t, f)
	}
}

// checkNestedCuts cancels f at every byte, in reads of 1, 7 and 4096 bytes and
// every cancelling-read mode, and holds every run to the sweep's terms: the
// input is not read after the cancelling read, a part that ended before it is
// what the cut reference delivers, and the part that ended after it ends with
// the stop, having delivered a prefix of the cut reference's body.
func checkNestedCuts(t *testing.T, f sweepFixture) {
	t.Helper()
	cut := &sweepCutReference{f: f, cache: map[int][]sweepPart{}}
	for _, chunk := range []int{1, 7, 4096} {
		for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
			for at := 1; at <= len(f.text); at++ {
				name := fmt.Sprintf("%s chunk %d %s at %d", f.name, chunk, mode, at)
				ctx, src := newCut(t, f.text, chunk, at, mode)
				session, e, _ := ParseContext(ctx, src, nil)
				var parts []sweepPart
				if e != nil {
					parts, _ = sweepConsume(e, "", func() bool { return src.cancelled })
				}
				if src.after != 0 {
					t.Fatalf("%s: the input was read %d times after the cancelling read", name, src.after)
				}
				if !src.cancelled || e == nil {
					continue
				}
				stop := error(session.fatalErr())
				if stop == nil {
					t.Fatalf("%s: cancelled with no stop", name)
				}
				ref := cut.at(src.pos)
				for i, p := range parts {
					if i >= len(ref) || p.path != ref[i].path {
						t.Fatalf("%s: part %d is %s, not in the cut reference %d parts", name, i, p.path, len(ref))
					}
					switch {
					case p.cancelled:
						if p.err != stop || !strings.HasPrefix(ref[i].body, p.body) {
							t.Fatalf("%s: part %s delivered (%q, %v), want a prefix of %q and the stop",
								name, p.path, p.body, p.err, ref[i].body)
						}
					case p.err != nil || p.body != ref[i].body:
						t.Fatalf("%s: part %s ended before the cancellation with (%q, %v), want %q",
							name, p.path, p.body, p.err, ref[i].body)
					}
				}
			}
		}
	}
}
