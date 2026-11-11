package mail

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/internal/scaletest"
)

// pfWideTree returns the tree of a container of media type kind with count
// leaves, built by hand so that classifying it is all a test of it measures.
func pfWideTree(kind string, params map[string]string, count int) *Node {
	root := &Node{MediaType: kind, Params: params}
	for i := 0; i < count; i++ {
		leaf := &Node{Parent: root, MediaType: "image/png", ContentID: fmt.Sprintf("img%06d@example.com", i), Data: []byte{0}}
		switch i % 4 {
		case 1:
			leaf.MediaType = "message/delivery-status"
		case 2:
			leaf.MediaType = "text/plain"
		case 3:
			leaf.MediaType, leaf.ContentID = "application/pgp-signature", ""
		}
		root.Children = append(root.Children, leaf)
	}
	return root
}

// pfTreeKinds are the containers whose parts roleOf has to place among their
// siblings, and one it does not.
var pfTreeKinds = []struct {
	name   string
	kind   string
	params map[string]string
}{
	{"mixed", "multipart/mixed", nil},
	{"signed", "multipart/signed", nil},
	{"encrypted", "multipart/encrypted", nil},
	{"report", "multipart/report", nil},
	{"related", "multipart/related", nil},
	// A start parameter that names no part: every part used to be compared
	// with it for every part.
	{"related-start-unmatched", "multipart/related", map[string]string{"start": "<nothing@example.com>"}},
	{"related-start-last", "multipart/related", map[string]string{"start": "<img000096@example.com>"}},
}

// TestClassifyMatchesRoleOf checks that classifying a tree gives every node
// the role roleOf gives it alone.
func TestClassifyMatchesRoleOf(t *testing.T) {
	for _, k := range pfTreeKinds {
		t.Run(k.name, func(t *testing.T) {
			// Nested, so that a part is also looked for under a root part that
			// is itself a container.
			root := pfWideTree(k.kind, k.params, 100)
			inner := pfWideTree("multipart/alternative", nil, 5)
			inner.Parent = root
			root.Children[0] = inner
			(&treeBuilder{}).classify(root)
			nodes := 0
			root.Walk(func(n *Node) bool {
				role, reason := roleOf(n)
				if n.Role != role || n.Reason != reason {
					t.Errorf("node %q under %q classified %v (%s), roleOf says %v (%s)", n.MediaType, k.kind, n.Role, n.Reason, role, reason)
				}
				nodes++
				return true
			})
			if nodes != 106 {
				t.Errorf("walked %d nodes, want 106", nodes)
			}
		})
	}
	// Where a part stands is read by position: the second part of a signed
	// entity is the signature, the first of an encrypted one the control part,
	// and the first of a report is never the report.
	signed := pfWideTree("multipart/signed", nil, 3)
	(&treeBuilder{}).classify(signed)
	for i, c := range signed.Children {
		if got := c.Role == RoleSignature; got != (i == 1) {
			t.Errorf("part %d of multipart/signed has role %v", i, c.Role)
		}
	}
	// The root part of a multipart/related is the one start names, and is no
	// resource of it; the parts with a Content-ID beside it are.
	related := pfWideTree("multipart/related", map[string]string{"start": "<img000004@example.com>"}, 8)
	(&treeBuilder{}).classify(related)
	for i, c := range related.Children {
		if got := c.Role == RoleRelated; got != (i != 4 && c.ContentID != "") {
			t.Errorf("part %d of multipart/related (Content-ID %q) has role %v", i, c.ContentID, c.Role)
		}
	}
	report := pfWideTree("multipart/report", nil, 6)
	report.Children[0].MediaType = "message/delivery-status"
	(&treeBuilder{}).classify(report)
	for i, c := range report.Children {
		if got := c.Role == RoleReport; got != (i > 0 && c.MediaType == "message/delivery-status") {
			t.Errorf("part %d of multipart/report (%s) has role %v", i, c.MediaType, c.Role)
		}
	}
}

// TestComplexityClassify holds the classification of a tree to the number of
// its nodes: a container of n parts used to cost n*n.
func TestComplexityClassify(t *testing.T) {
	for _, k := range pfTreeKinds {
		t.Run(k.name, func(t *testing.T) {
			n := 2000
			if scaletest.RaceEnabled {
				n = 500
			}
			// The input stands for the number of parts. The trees are built
			// beforehand: classifying one again gives each node the role it
			// has, and is all that is measured.
			trees := map[int]*Node{n: pfWideTree(k.kind, k.params, n), 4 * n: pfWideTree(k.kind, k.params, 4*n)}
			scaletest.Check(t, make([]byte, n), make([]byte, 4*n), func(parts []byte) {
				(&treeBuilder{}).classify(trees[len(parts)])
			})
		})
	}
}

// pfWideMessage returns a message that is one container of count small parts.
func pfWideMessage(kind, params string, count int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: a@example.com\r\nMIME-Version: 1.0\r\nContent-Type: multipart/%s; boundary=\"wide\"%s\r\n\r\n", kind, params)
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "--wide\r\nContent-Type: image/png\r\nContent-ID: <img%06d@example.com>\r\n\r\npart %d\r\n", i, i)
	}
	b.WriteString("--wide--\r\n")
	return b.Bytes()
}

var pfWideMessages = []struct {
	name, kind, params string
}{
	{"mixed", "mixed", ""},
	{"related-start-unmatched", "related", "; start=\"<nothing@example.com>\""},
	{"report", "report", "; report-type=delivery-status"},
}

// TestComplexityReadTree holds Parse, ReadTree and the views of the tree to
// the size of a message of many parts, and the flat Reader with them.
func TestComplexityReadTree(t *testing.T) {
	for _, c := range pfWideMessages {
		for _, view := range []string{"tree", "reader"} {
			t.Run(c.name+"/"+view, func(t *testing.T) {
				n := 600
				if scaletest.RaceEnabled {
					n = 150
				}
				scaletest.Check(t, pfWideMessage(c.kind, c.params, n), pfWideMessage(c.kind, c.params, 4*n), func(msg []byte) {
					_, e, err := message.Parse(bytes.NewReader(msg), nil)
					if e == nil {
						t.Fatalf("Parse: %v", err)
					}
					if view == "reader" {
						r := NewReader(e)
						parts := 0
						for {
							p, err := r.NextPart()
							if err != nil {
								break
							}
							io.Copy(io.Discard, p.Body)
							parts++
						}
						if parts < bytes.Count(msg, []byte("--wide\r\n")) {
							t.Fatalf("the reader returned %d parts", parts)
						}
						return
					}
					tree, err := ReadTree(e, nil)
					if err != nil {
						t.Fatalf("ReadTree: %v", err)
					}
					if got, want := len(tree.Root.Children), bytes.Count(msg, []byte("--wide\r\n")); got != want {
						t.Fatalf("the tree has %d parts, want %d", got, want)
					}
					tree.Root.Bodies("text/html", "text/plain")
					tree.Root.Attachments()
				})
			})
		}
	}
}

// TestReadTreeLeafCost holds what Parse and ReadTree allocate for one small
// leaf: each used to take a read buffer of 32 KiB of its own.
func TestReadTreeLeafCost(t *testing.T) {
	if scaletest.RaceEnabled {
		t.Skip("a sync.Pool drops what it is given at random under the race detector")
	}
	const parts = 1000
	msg := pfWideMessage("mixed", "", parts)
	read := func() {
		_, e, err := message.Parse(bytes.NewReader(msg), nil)
		if e == nil {
			t.Fatalf("Parse: %v", err)
		}
		if tree, err := ReadTree(e, nil); err != nil || len(tree.Root.Children) != parts {
			t.Fatalf("ReadTree: %v", err)
		}
	}
	read()
	if per := scaletest.Measure(1, read).Bytes / parts; per > 16<<10 {
		t.Errorf("a small part costs %d bytes of allocation, want at most %d", per, 16<<10)
	}
}

// BenchmarkReadTree parses a message and reads its whole structure: an
// ordinary one, and containers of 5000 parts.
func BenchmarkReadTree(b *testing.B) {
	typical := []byte("From: a@example.com\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain text\r\n" +
		"--inner\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html</p>\r\n--inner--\r\n" +
		"--outer\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--outer--\r\n")
	inputs := []struct {
		name string
		msg  []byte
	}{{"typical", typical}}
	for _, c := range pfWideMessages {
		inputs = append(inputs, struct {
			name string
			msg  []byte
		}{"wide-5000/" + c.name, pfWideMessage(c.kind, c.params, 5000)})
	}
	for _, in := range inputs {
		b.Run(in.name, func(b *testing.B) {
			r := bytes.NewReader(in.msg)
			b.SetBytes(int64(len(in.msg)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r.Reset(in.msg)
				_, e, err := message.Parse(r, nil)
				if e == nil {
					b.Fatal(err)
				}
				if _, err := ReadTree(e, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
