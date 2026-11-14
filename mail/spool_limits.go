package mail

import (
	"sync"

	"github.com/saeroun-ai/emlkit"
)

// The parse's spool budgets (#43, #70).
//
// Limits.MaxMemorySpoolBytes and Limits.MaxDiskSpoolBytes bound what one
// message keeps in memory and outside it. ReadTree reads them from the
// entity's ParseSession and gives every node of the tree it builds the same
// spoolBudget, which counts what any Spool kept for the bodies of that tree.
// Spool.Body applies the smaller of the room the budget has left and the room
// its own SpoolOptions quota has left, so neither source of a positive quota
// is ignored or loosened:
//
//   - A positive budget bounds the tree's bodies together. LimitDisabled
//     bounds nothing, and the spool's own quota alone applies.
//   - A negative SpoolOptions quota keeps nothing there, whatever the parse
//     allows.
//   - A node with no budget — the tree of an entity of Read, ReadWithOptions
//     or New, which has no session, or a Node built by hand — is spooled under
//     the SpoolOptions alone, as before.
//
// The budget is the tree's, not the spool's: one spool used for two trees
// gives each its own MaxMemorySpoolBytes, and its own quotas count both.
type spoolBudget struct {
	mu        sync.Mutex
	mem, disk int64 // the resolved budgets: positive, or LimitDisabled
	memUsed   int64
	diskUsed  int64
}

// newSpoolBudget returns the budget of a parse, or nil for an entity no
// session saw.
func newSpoolBudget(e *message.Entity) *spoolBudget {
	s := e.ParseSession()
	if s == nil {
		return nil
	}
	l := s.Limits()
	return &spoolBudget{mem: l.MaxMemorySpoolBytes, disk: l.MaxDiskSpoolBytes}
}

// room returns how much of each budget is left, or -1 for one that bounds
// nothing.
func (b *spoolBudget) room() (mem, disk int64) {
	mem, disk = -1, -1
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mem > 0 {
		mem = max(b.mem-b.memUsed, 0)
	}
	if b.disk > 0 {
		disk = max(b.disk-b.diskUsed, 0)
	}
	return
}

func (b *spoolBudget) charge(mem, disk int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.memUsed += mem
	b.diskUsed += disk
	b.mu.Unlock()
}
