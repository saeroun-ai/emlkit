package smime

// Limits are the budgets one call reads under. The zero value, and a nil
// *Limits, select the default of every budget; so does a zero or negative
// field. There is no unlimited setting: a caller that wants more sets more.
type Limits struct {
	// MaxInputBytes is how much of the input is read. More than that is cut
	// off, reported, and read as a truncated object. Default 64 MiB.
	MaxInputBytes int

	// MaxDepth is how deep elements may nest. Default 32, at most 128.
	MaxDepth int

	// MaxElements is how many element headers one call may read, counting
	// each time one is read. It bounds the work a hostile nesting of
	// indefinite lengths can ask for. Default 1<<20.
	MaxElements int

	// MaxContentBytes is how large a reassembled content — the eContent of a
	// SignedData, the encrypted content of an EnvelopedData — may be. What is
	// past it is dropped and reported. Default 64 MiB.
	MaxContentBytes int

	// MaxInflatedBytes is how much a CompressedData may inflate to. Default
	// 16 MiB.
	MaxInflatedBytes int

	// MaxCertificates is how many certificates of one SignedData are kept and
	// parsed; default 64. MaxCertificateBytes is the size past which one is
	// kept raw and not parsed; default 64 KiB.
	MaxCertificates     int
	MaxCertificateBytes int

	// MaxSigners and MaxRecipients bound the signer and recipient infos that
	// are kept; defaults 16 and 256. MaxAttributes bounds the attributes of
	// one signer; default 64.
	MaxSigners    int
	MaxRecipients int
	MaxAttributes int

	// MaxIssues is how many issues one call reports; the one that reaches it
	// is replaced by limit.issues. Default 100.
	MaxIssues int
}

const (
	defaultMaxInput      = 64 << 20
	defaultMaxDepth      = 32
	hardMaxDepth         = 128
	defaultMaxElements   = 1 << 20
	defaultMaxContent    = 64 << 20
	defaultMaxInflated   = 16 << 20
	defaultMaxCerts      = 64
	defaultMaxCertBytes  = 64 << 10
	defaultMaxSigners    = 16
	defaultMaxRecipients = 256
	defaultMaxAttributes = 64
	defaultMaxIssues     = 100

	// maxSmallOctets bounds the octet strings that are never content:
	// signatures, digests, key identifiers, encrypted keys.
	maxSmallOctets = 64 << 10
	// maxIndefiniteMemo bounds the remembered ends of indefinite elements.
	maxIndefiniteMemo = 1 << 12
	// maxArmorRounds is how many times an input may be Base64 inside Base64.
	maxArmorRounds = 3
)

// pick returns the budget a field selects: its default when it is not
// positive, and never more than fits 31 bits, so that no sum of a budget and a
// small number overflows.
func pick(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > 1<<31-1 {
		return 1<<31 - 1
	}
	return v
}

// resolve returns the limits with every default filled in.
func (l *Limits) resolve() Limits {
	var r Limits
	if l != nil {
		r = *l
	}
	r.MaxInputBytes = pick(r.MaxInputBytes, defaultMaxInput)
	r.MaxDepth = pick(r.MaxDepth, defaultMaxDepth)
	if r.MaxDepth > hardMaxDepth {
		r.MaxDepth = hardMaxDepth
	}
	r.MaxElements = pick(r.MaxElements, defaultMaxElements)
	r.MaxContentBytes = pick(r.MaxContentBytes, defaultMaxContent)
	r.MaxInflatedBytes = pick(r.MaxInflatedBytes, defaultMaxInflated)
	r.MaxCertificates = pick(r.MaxCertificates, defaultMaxCerts)
	r.MaxCertificateBytes = pick(r.MaxCertificateBytes, defaultMaxCertBytes)
	r.MaxSigners = pick(r.MaxSigners, defaultMaxSigners)
	r.MaxRecipients = pick(r.MaxRecipients, defaultMaxRecipients)
	r.MaxAttributes = pick(r.MaxAttributes, defaultMaxAttributes)
	r.MaxIssues = pick(r.MaxIssues, defaultMaxIssues)
	return r
}
