package mail_test

import (
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// The two defects of the structure view and the spool that the bounds audit
// found and did not fix, because mail/tree.go and mail/spool.go were not its
// to change (ROB-120-001; docs/robustness-notes/bounds-audit-values.md, "Left
// open"). Both subtests fail as the code stands, the first by a goroutine that
// never ends, so they run only when asked for:
//
//	EMLKIT_BV_OPEN=1 go test -run TestBVOpen ./mail/
//
// Whoever fixes the two files removes the gate.
func TestBVOpen_treeAndSpool(t *testing.T) {
	if os.Getenv("EMLKIT_BV_OPEN") == "" {
		t.Skip("known open defects of mail/tree.go and mail/spool.go; set EMLKIT_BV_OPEN=1 to run")
	}

	// A source that fails inside a multipart: its iterator then returns no
	// part and a newly wrapped error on every call, which ReadTree does not
	// recognize as the same one, and asks again for ever.
	t.Run("ReadTree ends when the source fails", func(t *testing.T) {
		for _, body := range []string{"", "--M\r\nContent-Type: text/pl", "--M\r\nContent-Type: text/plain\r\n\r\nabc"} {
			src := &bvFailingSource{r: strings.NewReader("Content-Type: multipart/mixed; boundary=M\r\n\r\n" + body)}
			_, e, err := message.Parse(src, nil)
			if e == nil {
				t.Fatalf("Parse() = %v", err)
			}
			done := make(chan struct{})
			go func() {
				mail.ReadTree(e, nil)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("ReadTree over a source that fails after %q did not return", body)
			}
		}
	})

	// A memory quota of math.MaxInt64 overflows the "one byte more" the spool
	// reads to see whether a body fits, and every body is kept as empty.
	t.Run("Spool keeps a body under the largest quota", func(t *testing.T) {
		_, e, err := message.Parse(strings.NewReader("Content-Type: text/plain\r\n\r\nhello world\r\n"), nil)
		if e == nil {
			t.Fatalf("Parse() = %v", err)
		}
		spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: math.MaxInt64})
		defer spool.Close()
		tree, err := mail.ReadTree(e, &mail.TreeOptions{Body: spool.Body})
		if err != nil {
			t.Fatalf("ReadTree() = %v", err)
		}
		if kept, cut, ok := spool.Kept(tree.Root); !ok || cut || kept != tree.Root.Size {
			t.Errorf("spool keeps %d bytes (cut %v) of a body of %d", kept, cut, tree.Root.Size)
		}
	})
}

// A bvFailingSource fails where its reader would end.
type bvFailingSource struct{ r io.Reader }

func (f *bvFailingSource) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		err = errors.New("source failed")
	}
	return n, err
}
