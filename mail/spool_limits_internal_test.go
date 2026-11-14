package mail

import (
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
)

// Which nodes carry the parse's spool budget, and with which values (#43).
func TestSpoolBudget_carried(t *testing.T) {
	const msg = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nx\r\n--B--\r\n"

	// A parse configured with nothing has the defaults, never zero.
	_, e, err := message.Parse(strings.NewReader(msg), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ReadTree(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := tree.Root.Children[0].budget
	def := message.DefaultLimits()
	if b == nil || b != tree.Root.budget || b.mem != def.MaxMemorySpoolBytes || b.disk != def.MaxDiskSpoolBytes {
		t.Fatalf("budget %+v, want the defaults shared by the tree", b)
	}

	// LimitDisabled bounds nothing.
	_, e, err = message.Parse(strings.NewReader(msg), &message.ParseOptions{Limits: message.Limits{
		MaxMemorySpoolBytes: message.LimitDisabled, MaxDiskSpoolBytes: message.LimitDisabled}})
	if err != nil {
		t.Fatal(err)
	}
	if tree, err = ReadTree(e, nil); err != nil {
		t.Fatal(err)
	}
	if mem, disk := tree.Root.Children[0].budget.room(); mem != -1 || disk != -1 {
		t.Errorf("disabled budgets have room %d, %d; want none", mem, disk)
	}

	// An entity of Read has no session and no budget.
	re, err := message.Read(strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	if tree, err = ReadTree(re, nil); err != nil {
		t.Fatal(err)
	}
	if tree.Root.Children[0].budget != nil {
		t.Error("a node of Read carries a parse budget")
	}
	var none *spoolBudget
	if mem, disk := none.room(); mem != -1 || disk != -1 {
		t.Errorf("no budget has room %d, %d", mem, disk)
	}

	// A spent budget has no room, which is not the same as no budget.
	spent := &spoolBudget{mem: 5, disk: 7}
	spent.charge(5, 9)
	if mem, disk := spent.room(); mem != 0 || disk != 0 {
		t.Errorf("spent budget room %d, %d; want 0, 0", mem, disk)
	}
}
