package pgpmime

import "fmt"

// Limits bounds the work and memory of one Parse, ParsePackets or request
// helper call. A zero or negative field takes its default. The budgets are
// shared by everything the call reads: packets inside compressed packets
// count toward the same MaxPackets as those outside.
type Limits struct {
	// MaxInputBytes is how much of the input is read; the rest is ignored
	// with a limit.input issue. Default 32 MiB.
	MaxInputBytes int64
	// MaxBlocks is how many armored blocks are decoded. Default 64.
	MaxBlocks int
	// MaxArmorHeaders is how many armor headers one block keeps. Default 32.
	MaxArmorHeaders int
	// MaxPackets is how many packets are walked. Default 4096.
	MaxPackets int
	// MaxKeptBytes is how many bytes of packet content are copied and kept:
	// literal data, user IDs, signer user IDs. Default 4 MiB.
	MaxKeptBytes int64
	// MaxDecompressedBytes is how many bytes compressed packets may inflate
	// to, all together. Default 32 MiB.
	MaxDecompressedBytes int64
	// MaxDepth is how deep compressed packets are inflated inside each
	// other. Default 8.
	MaxDepth int
	// MaxSubpackets is how many subpackets of one signature are read.
	// Default 64.
	MaxSubpackets int
	// MaxUserIDs is how many user ID packets are kept. Default 64.
	MaxUserIDs int
	// MaxIssues is how many issues are recorded; one limit.issues issue
	// marks that more were dropped. Default 256.
	MaxIssues int
}

// DefaultLimits returns the limits a nil *Limits stands for.
func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes:        32 << 20,
		MaxBlocks:            64,
		MaxArmorHeaders:      32,
		MaxPackets:           4096,
		MaxKeptBytes:         4 << 20,
		MaxDecompressedBytes: 32 << 20,
		MaxDepth:             8,
		MaxSubpackets:        64,
		MaxUserIDs:           64,
		MaxIssues:            256,
	}
}

func (l *Limits) resolve() Limits {
	d := DefaultLimits()
	if l == nil {
		return d
	}
	r := *l
	if r.MaxInputBytes <= 0 {
		r.MaxInputBytes = d.MaxInputBytes
	}
	if r.MaxBlocks <= 0 {
		r.MaxBlocks = d.MaxBlocks
	}
	if r.MaxArmorHeaders <= 0 {
		r.MaxArmorHeaders = d.MaxArmorHeaders
	}
	if r.MaxPackets <= 0 {
		r.MaxPackets = d.MaxPackets
	}
	if r.MaxKeptBytes <= 0 {
		r.MaxKeptBytes = d.MaxKeptBytes
	}
	if r.MaxDecompressedBytes <= 0 {
		r.MaxDecompressedBytes = d.MaxDecompressedBytes
	}
	if r.MaxDepth <= 0 {
		r.MaxDepth = d.MaxDepth
	}
	if r.MaxSubpackets <= 0 {
		r.MaxSubpackets = d.MaxSubpackets
	}
	if r.MaxUserIDs <= 0 {
		r.MaxUserIDs = d.MaxUserIDs
	}
	if r.MaxIssues <= 0 {
		r.MaxIssues = d.MaxIssues
	}
	return r
}

// state is the shared budget of one call.
type state struct {
	lim      Limits
	packets  int
	kept     int64
	inflated int64
	userIDs  int
	issues   int
	capped   bool
	// userIDCapped and keptCapped record that the limit issue was given.
	userIDCapped, keptCapped bool
}

func newState(l *Limits) *state {
	return &state{lim: l.resolve()}
}

// add appends an issue to dst unless the issue budget is spent.
func (s *state) add(dst *[]Issue, code Code, off int64, format string, args ...any) {
	if s.issues >= s.lim.MaxIssues {
		if !s.capped {
			s.capped = true
			*dst = append(*dst, Issue{Code: CodeIssueLimit, Offset: -1,
				Message: fmt.Sprintf("more than %d issues; the rest are not recorded", s.lim.MaxIssues)})
		}
		return
	}
	s.issues++
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	*dst = append(*dst, Issue{Code: code, Offset: off, Message: msg})
}

// keep reserves up to n bytes of the kept-bytes budget and returns how many
// were granted.
func (s *state) keep(n int64) int64 {
	left := s.lim.MaxKeptBytes - s.kept
	if left <= 0 || n <= 0 {
		return 0
	}
	if n > left {
		n = left
	}
	s.kept += n
	return n
}
