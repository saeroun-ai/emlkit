package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"sort"
	"strings"
)

type headerField struct {
	b []byte // Raw header field, including whitespace

	k string
	v string
}

func newHeaderField(k, v string, b []byte) *headerField {
	return &headerField{k: textproto.CanonicalMIMEHeaderKey(k), v: v, b: b}
}

func (f *headerField) raw() ([]byte, error) {
	if f.b != nil {
		return f.b, nil
	} else {
		for pos, ch := range f.k {
			// check if character is a printable US-ASCII except ':'
			if !(ch >= '!' && ch < ':' || ch > ':' && ch <= '~') {
				return nil, fmt.Errorf("field name contains incorrect symbols (\\x%x at %v)", ch, pos)
			}
		}

		if pos := strings.IndexAny(f.v, "\r\n"); pos != -1 {
			return nil, fmt.Errorf("field value contains \\r\\n (at %v)", pos)
		}

		return []byte(formatHeaderField(f.k, f.v)), nil
	}
}

// A Header represents the key-value pairs in a message header.
//
// The header representation is idempotent: if the header can be read and
// written, the result will be exactly the same as the original (including
// whitespace and header field ordering). This is required for e.g. DKIM.
//
// Mutating the header is restricted: the only two allowed operations are
// inserting a new header field at the top and deleting a header field. This is
// again necessary for DKIM.
type Header struct {
	// Fields are in reverse order so that inserting a new field at the top is
	// cheap.
	l []*headerField
	m map[string][]*headerField
}

func makeHeaderMap(fs []*headerField) map[string][]*headerField {
	if len(fs) == 0 {
		return nil
	}

	m := make(map[string][]*headerField, len(fs))
	for i, f := range fs {
		m[f.k] = append(m[f.k], fs[i])
	}
	return m
}

func newHeader(fs []*headerField) Header {
	// Reverse order
	for i := len(fs)/2 - 1; i >= 0; i-- {
		opp := len(fs) - 1 - i
		fs[i], fs[opp] = fs[opp], fs[i]
	}

	return Header{l: fs, m: makeHeaderMap(fs)}
}

// HeaderFromMap creates a header from a map of header fields.
//
// This function is provided for interoperability with the standard library.
// If possible, ReadHeader should be used instead to avoid loosing information.
// The map representation looses the ordering of the fields, the capitalization
// of the header keys, and the whitespace of the original header.
func HeaderFromMap(m map[string][]string) Header {
	fs := make([]*headerField, 0, len(m))
	for k, vs := range m {
		for _, v := range vs {
			fs = append(fs, newHeaderField(k, v, nil))
		}
	}

	sort.SliceStable(fs, func(i, j int) bool {
		return fs[i].k < fs[j].k
	})

	return newHeader(fs)
}

// AddRaw adds the raw key, value pair to the header.
//
// The supplied byte slice should be a complete field in the "Key: Value" form
// including trailing CRLF. If there is no colon in the input - AddRaw panics.
// No changes are made to kv contents and it will be copied into WriteHeader
// output as is.
//
// kv is directly added to the underlying structure and therefore should not be
// modified after the AddRaw call.
func (h *Header) AddRaw(kv []byte) {
	colon := bytes.IndexByte(kv, ':')
	if colon == -1 {
		panic("textproto: Header.AddRaw: missing colon")
	}
	k := textproto.CanonicalMIMEHeaderKey(string(trim(kv[:colon])))
	v := trimAroundNewlines(kv[colon+1:])

	if h.m == nil {
		h.m = make(map[string][]*headerField)
	}

	f := newHeaderField(k, v, kv)
	h.l = append(h.l, f)
	h.m[k] = append(h.m[k], f)
}

// Add adds the key, value pair to the header. It prepends to any existing
// fields associated with key.
//
// Key and value should obey character requirements of RFC 6532.
// If you need to format or fold lines manually, use AddRaw.
func (h *Header) Add(k, v string) {
	k = textproto.CanonicalMIMEHeaderKey(k)

	if h.m == nil {
		h.m = make(map[string][]*headerField)
	}

	f := newHeaderField(k, v, nil)
	h.l = append(h.l, f)
	h.m[k] = append(h.m[k], f)
}

// Get gets the first value associated with the given key. If there are no
// values associated with the key, Get returns "".
func (h *Header) Get(k string) string {
	fields := h.m[textproto.CanonicalMIMEHeaderKey(k)]
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1].v
}

// Raw gets the first raw header field associated with the given key.
//
// The returned bytes contain a complete field in the "Key: value" form,
// including trailing CRLF.
//
// The returned slice should not be modified and becomes invalid when the
// header is updated.
//
// An error is returned if the header field contains incorrect characters (see
// RFC 6532).
func (h *Header) Raw(k string) ([]byte, error) {
	fields := h.m[textproto.CanonicalMIMEHeaderKey(k)]
	if len(fields) == 0 {
		return nil, nil
	}
	return fields[len(fields)-1].raw()
}

// Values returns all values associated with the given key.
//
// The returned slice should not be modified and becomes invalid when the
// header is updated.
func (h *Header) Values(k string) []string {
	fields := h.m[textproto.CanonicalMIMEHeaderKey(k)]
	if len(fields) == 0 {
		return nil
	}
	l := make([]string, len(fields))
	for i, field := range fields {
		l[len(fields)-i-1] = field.v
	}
	return l
}

// Set sets the header fields associated with key to the single field value.
// It replaces any existing values associated with key.
func (h *Header) Set(k, v string) {
	h.Del(k)
	h.Add(k, v)
}

// Del deletes the values associated with key.
func (h *Header) Del(k string) {
	k = textproto.CanonicalMIMEHeaderKey(k)

	delete(h.m, k)

	// Delete existing keys
	for i := len(h.l) - 1; i >= 0; i-- {
		if h.l[i].k == k {
			h.l = append(h.l[:i], h.l[i+1:]...)
		}
	}
}

// Has checks whether the header has a field with the specified key.
func (h *Header) Has(k string) bool {
	_, ok := h.m[textproto.CanonicalMIMEHeaderKey(k)]
	return ok
}

// Copy creates an independent copy of the header.
func (h *Header) Copy() Header {
	l := make([]*headerField, len(h.l))
	copy(l, h.l)
	m := makeHeaderMap(l)
	return Header{l: l, m: m}
}

// Len returns the number of fields in the header.
func (h *Header) Len() int {
	return len(h.l)
}

// Map returns all header fields as a map.
//
// This function is provided for interoperability with the standard library.
// If possible, Fields should be used instead to avoid loosing information.
// The map representation looses the ordering of the fields, the capitalization
// of the header keys, and the whitespace of the original header.
func (h *Header) Map() map[string][]string {
	m := make(map[string][]string, h.Len())
	fields := h.Fields()
	for fields.Next() {
		m[fields.Key()] = append(m[fields.Key()], fields.Value())
	}
	return m
}

// HeaderFields iterates over header fields. Its cursor starts before the first
// field of the header. Use Next to advance from field to field.
type HeaderFields interface {
	// Next advances to the next header field. It returns true on success, or
	// false if there is no next field.
	Next() (more bool)
	// Key returns the key of the current field.
	Key() string
	// Value returns the value of the current field.
	Value() string
	// Raw returns the raw current header field. See Header.Raw.
	Raw() ([]byte, error)
	// Del deletes the current field.
	Del()
	// Len returns the amount of header fields in the subset of header iterated
	// by this HeaderFields instance.
	//
	// For Fields(), it will return the amount of fields in the whole header section.
	// For FieldsByKey(), it will return the amount of fields with certain key.
	Len() int

	headerFields()
}

type headerFields struct {
	h   *Header
	cur int
}

func (*headerFields) headerFields() {}

func (fs *headerFields) Next() bool {
	fs.cur++
	return fs.cur < len(fs.h.l)
}

func (fs *headerFields) index() int {
	if fs.cur < 0 {
		panic("message: HeaderFields method called before Next")
	}
	if fs.cur >= len(fs.h.l) {
		panic("message: HeaderFields method called after Next returned false")
	}
	return len(fs.h.l) - fs.cur - 1
}

func (fs *headerFields) field() *headerField {
	return fs.h.l[fs.index()]
}

func (fs *headerFields) Key() string {
	return fs.field().k
}

func (fs *headerFields) Value() string {
	return fs.field().v
}

func (fs *headerFields) Raw() ([]byte, error) {
	return fs.field().raw()
}

func (fs *headerFields) Del() {
	f := fs.field()

	ok := false
	for i, ff := range fs.h.m[f.k] {
		if ff == f {
			ok = true
			fs.h.m[f.k] = append(fs.h.m[f.k][:i], fs.h.m[f.k][i+1:]...)
			if len(fs.h.m[f.k]) == 0 {
				delete(fs.h.m, f.k)
			}
			break
		}
	}
	if !ok {
		panic("message: field not found in Header.m")
	}

	fs.h.l = append(fs.h.l[:fs.index()], fs.h.l[fs.index()+1:]...)
	fs.cur--
}

func (fs *headerFields) Len() int {
	return len(fs.h.l)
}

// Fields iterates over all the header fields.
//
// The header may not be mutated while iterating, except using HeaderFields.Del.
func (h *Header) Fields() HeaderFields {
	return &headerFields{h, -1}
}

type headerFieldsByKey struct {
	h   *Header
	k   string
	cur int
}

func (*headerFieldsByKey) headerFields() {}

func (fs *headerFieldsByKey) Next() bool {
	fs.cur++
	return fs.cur < len(fs.h.m[fs.k])
}

func (fs *headerFieldsByKey) index() int {
	if fs.cur < 0 {
		panic("message: headerfields.key or value called before next")
	}
	if fs.cur >= len(fs.h.m[fs.k]) {
		panic("message: headerfields.key or value called after next returned false")
	}
	return len(fs.h.m[fs.k]) - fs.cur - 1
}

func (fs *headerFieldsByKey) field() *headerField {
	return fs.h.m[fs.k][fs.index()]
}

func (fs *headerFieldsByKey) Key() string {
	return fs.field().k
}

func (fs *headerFieldsByKey) Value() string {
	return fs.field().v
}

func (fs *headerFieldsByKey) Raw() ([]byte, error) {
	return fs.field().raw()
}

func (fs *headerFieldsByKey) Del() {
	f := fs.field()

	ok := false
	for i := range fs.h.l {
		if f == fs.h.l[i] {
			ok = true
			fs.h.l = append(fs.h.l[:i], fs.h.l[i+1:]...)
			break
		}
	}
	if !ok {
		panic("message: field not found in Header.l")
	}

	fs.h.m[fs.k] = append(fs.h.m[fs.k][:fs.index()], fs.h.m[fs.k][fs.index()+1:]...)
	if len(fs.h.m[fs.k]) == 0 {
		delete(fs.h.m, fs.k)
	}
	fs.cur--
}

func (fs *headerFieldsByKey) Len() int {
	return len(fs.h.m[fs.k])
}

// FieldsByKey iterates over all fields having the specified key.
//
// The header may not be mutated while iterating, except using HeaderFields.Del.
func (h *Header) FieldsByKey(k string) HeaderFields {
	return &headerFieldsByKey{h, textproto.CanonicalMIMEHeaderKey(k), -1}
}

// ErrHeaderTooBig reports that a header block did not fit in the byte budget it
// was read under. Match it with errors.Is; see ReadHeaderLimit for what is
// counted, and HeaderLimitError for the error a refused block actually returns.
//
// It is the sentinel to test for, not the sentinel to produce. A budget refusal
// is recognized by the HeaderLimitError this package builds for it, so an
// io.Reader that returns this value — or wraps it — is an ordinary read failure
// and is reported as one, not mistaken for a budget that was never reached.
var ErrHeaderTooBig = errors.New("message: header exceeds maximum size")

// errBudgetExhausted is a header budget refusing a block, as opposed to
// anything else that could carry ErrHeaderTooBig.
//
// The distinction is the point of it. Classification used to ask whether an
// error was ErrHeaderTooBig, which an underlying reader can answer yes to by
// returning that documented sentinel itself; the answer to "did this parser's
// own budget stop this block" has to be one only this parser can give. This
// value is unexported and is created in exactly one place, so no reader can
// present it, while it wraps ErrHeaderTooBig so that everything documented to
// match a budget failure still matches.
//
// It is the one stop of every budget of HeaderLimits, and it says only that one
// of them stopped: which one, and where, is recorded in the headerScan that
// produced it, because that is state a reader cannot reach at all. The entry
// points below read the two together and turn them into a HeaderLimitError that
// says which budget and where.
//
// It does not leave this package.
var errBudgetExhausted = fmt.Errorf("%w: byte budget exhausted", ErrHeaderTooBig)

// isBudgetExhausted reports whether err is this package's own budget refusing a
// block.
//
// The comparison is exact, and deliberately not errors.Is. errors.Is asks each
// error in a chain whether it "is" the target, and an error is free to answer
// yes to anything: an Is(error) bool method that returns true claims every
// target, including one it could never have obtained. Provenance cannot be
// established by asking the suspect. errBudgetExhausted is created in exactly
// one place and travels unwrapped to every decision point that consults this,
// so identity is the whole test and needs no traversal to be fooled through.
func isBudgetExhausted(err error) bool {
	return err == errBudgetExhausted
}

// A HeaderLimitError reports that a header block exceeded one of the budgets it
// was read under, and says which budget, by how much it was allowed, and where
// in the reader's input the refusal happened.
//
// # Provenance
//
// It is what a budget refusal is, and only what a budget refusal is. A value
// carrying the refusal can be built in exactly one place, inside this package,
// because the fields that carry it are unexported: another package can write
// &HeaderLimitError{}, which is the zero value and refuses nothing, but it
// cannot produce one that claims a budget. HeaderLimit is the predicate that
// tells the two apart, and it is what a caller classifying a refused header
// should use.
//
// The one case that cannot be told apart is a genuine error, obtained from this
// package, presented again somewhere it did not come from. Nothing in the type
// system distinguishes a value from itself; a caller that hands a parser's own
// error back to the parser as an input failure gets what it asked for.
//
// # Reading one
//
// Kind says which budget of HeaderLimits was exceeded, Limit says what that
// budget was, and Offset and UnitOffset say where. It wraps the sentinel of its
// kind — ErrHeaderTooBig, ErrHeaderFieldTooBig, ErrTooManyHeaderFields or
// ErrLineTooBig — so errors.Is finds the budget failure through it:
//
//	if limit, ok := textproto.HeaderLimit(err); ok {
//		log.Printf("header at %d: %v budget of %d exceeded at %d",
//			limit.Offset(), limit.Kind(), limit.Limit(), limit.UnitOffset())
//	}
//
// errors.As reaches it too, but answers a different question — whether something
// shaped like this is in the chain, not whether this package refused a block —
// so it is the wrong tool for deciding that a budget was reached.
type HeaderLimitError struct {
	// fromParser marks a value this package created. It is unexported, and set
	// in exactly one place, so it is false in every HeaderLimitError assembled
	// anywhere else — which is what makes the zero value that another package
	// can write distinguishable from a refusal that really happened.
	fromParser bool

	// kind is which budget was exceeded.
	kind HeaderLimitKind

	// limit is that budget, in the unit the kind counts.
	limit int64

	// offset is where the refused block began.
	offset int64

	// unitOffset is where the unit the budget refused began: the physical line,
	// the logical field, or the block itself. See UnitOffset.
	unitOffset int64
}

// newHeaderLimitError returns the refusal of kind, with the budget limit, of a
// header block that began at offset, where the refused unit began at
// unitOffset. It is the only place a genuine HeaderLimitError is made.
func newHeaderLimitError(kind HeaderLimitKind, limit, offset, unitOffset int64) *HeaderLimitError {
	return &HeaderLimitError{
		fromParser: true,
		kind:       kind,
		limit:      limit,
		offset:     offset,
		unitOffset: unitOffset,
	}
}

// Kind returns which budget of HeaderLimits was exceeded. It returns
// HeaderLimitBytes for a nil error, which is that type's zero value.
func (e *HeaderLimitError) Kind() HeaderLimitKind {
	if e == nil {
		return HeaderLimitBytes
	}
	return e.kind
}

// Limit returns the budget that was exceeded, in the unit its kind counts:
// bytes for every kind but HeaderLimitCount, which counts fields. It returns 0
// for a nil error.
func (e *HeaderLimitError) Limit() int64 {
	if e == nil {
		return 0
	}
	return e.limit
}

// Offset returns the offset of the first byte of the refused header block,
// relative to the first byte of the input the reader was given, on the same
// terms as Part.HeaderOffset. It is a real position: the reader had already
// reached the block before it read a byte of it.
//
// For a block read by ReadHeaderLimit or ReadHeaderLimits the input is the
// bufio.Reader as it stood when the call began, so the block starts at 0. It
// returns 0 for a nil error.
func (e *HeaderLimitError) Offset() int64 {
	if e == nil {
		return 0
	}
	return e.unitOffsetOrBlock()
}

// UnitOffset returns the offset of the first byte of the unit the budget
// refused, on the same terms as Offset:
//
//   - for HeaderLimitLineBytes, the physical line that was too long;
//   - for HeaderLimitFieldBytes, the logical field that was too long, which is
//     the first byte of its first physical line;
//   - for HeaderLimitCount, the logical field that did not fit in the count;
//   - for HeaderLimitBytes, the block itself, so it equals Offset.
//
// It returns 0 for a nil error.
func (e *HeaderLimitError) UnitOffset() int64 {
	if e == nil {
		return 0
	}
	return e.unitOffset
}

// unitOffsetOrBlock returns the block offset. It exists so that Offset keeps
// meaning the block for every kind, including the ones whose UnitOffset is
// somewhere inside it.
func (e *HeaderLimitError) unitOffsetOrBlock() int64 {
	return e.offset
}

// Error implements error. The text names a header block rather than any one
// layer, because a block refused here may be a message's own header, a part's,
// or one a caller read directly.
//
// It is safe on a nil receiver: a nil of this type inside a non-nil error
// interface is something an adversarial input can present, and formatting what
// arrived must not be the thing that brings a parser down.
func (e *HeaderLimitError) Error() string {
	if e == nil {
		return "message: nil header limit error"
	}
	if e.kind == HeaderLimitBytes {
		return fmt.Sprintf("message: header block at offset %d exceeds the limit of %d bytes", e.offset, e.limit)
	}
	return fmt.Sprintf("message: header block at offset %d exceeds the %s limit of %d %s at offset %d",
		e.offset, e.kind, e.limit, e.kind.unit(), e.unitOffset)
}

// Unwrap reports the budget failure this error describes, so that errors.Is
// finds the sentinel of its kind through it, as for any other refused block.
//
// It unwraps to a public sentinel rather than to this package's private one.
// Handing the private value out through a type another package can write would
// give that package a way to present it, which is exactly what the private
// value exists to prevent.
func (e *HeaderLimitError) Unwrap() error {
	return e.Kind().sentinel()
}

// HeaderLimit returns the header budget refusal in err's chain, and whether
// there is a genuine one.
//
// This is the predicate for "did a header budget refuse a block", and the only
// one that answers it. It is not errors.As: errors.As reports that a value of
// this shape is present, and a value of this shape is something any package can
// write and any input can hand back — including a typed nil, which errors.As
// reports as found and leaves a caller to dereference. HeaderLimit reports only
// a non-nil value this package created, so what it returns is always safe to
// read.
//
// What is walked is the whole error tree, not one chain of it: both
// Unwrap() error and Unwrap() []error are followed, so a refusal joined with
// other errors by errors.Join is found wherever in the join it sits. Every node
// is matched by exact dynamic type and by the mark, and no Is or As method is
// ever called, so no error can claim to be a refusal by answering a question
// about itself.
//
// The walk is bounded in both directions an error graph can grow, because an
// input decides the shape of that graph and nothing here should be at its mercy:
//
//   - a cap on the nodes examined, which is what makes a cycle — an error that
//     unwraps to itself, or a ring of them — terminate rather than hang;
//   - a fixed number of children held pending, which is what keeps a join of a
//     million errors from costing a million slots. What does not fit is not
//     examined;
//   - a fixed number of child slots inspected across the whole walk, nil
//     children included, so that a join whose children are a long run of nils
//     is not scanned to its end either.
//
// So a refusal buried deeper or wider than those caps is not found, and is
// reported as no refusal rather than by searching without limit. Nothing the
// parser builds comes close: a refusal is returned as itself, or one wrap from
// the top.
//
// Nodes that are nil, or that hold a nil pointer, are never asked to unwrap
// themselves — what an input hands over may have methods that dereference
// nothing — and an Unwrap that panics is treated as having no children. The
// walk bounds how many Unwrap calls it makes, not what one of them does: an
// Unwrap that blocks or allocates without end is the caller's code and is not
// interrupted.
//
// The one value that cannot be told apart is a genuine refusal presented again
// somewhere it did not come from; see HeaderLimitError.
func HeaderLimit(err error) (*HeaderLimitError, bool) {
	var found *HeaderLimitError
	walkError(err, func(node error) bool {
		// Exact dynamic type, and then the mark. A HeaderLimitError this
		// package did not create carries no refusal, and neither does a nil one;
		// the walk goes on either way, since something genuine may still be
		// below.
		if e, ok := node.(*HeaderLimitError); ok && e != nil && e.fromParser {
			found = e
			return true
		}
		return false
	})
	return found, found != nil
}

// A headerRefusal is one budget of HeaderLimits refusing to admit a header
// block: which budget, what it was, and where in the block the unit it refused
// began.
//
// The offset is relative to the first byte of the block, because that is the
// only reference point this layer has. Whoever knows where the block itself sits
// in the input adds that, which is what turns it into a position a caller can
// use; see newHeaderLimitError.
type headerRefusal struct {
	kind   HeaderLimitKind
	limit  int64
	offset int64
}

// A headerScan is the accounting of one header block as it is read: how much of
// it has been consumed, where the current physical line and logical field
// began, and which of its budgets, if any, has refused it.
//
// The zero headerScan budgets nothing, which is what ReadHeader reads under, and
// still counts what it consumes, because the positions a refusal reports are
// measured from the start of the block whether or not that particular budget is
// on.
//
// It is per block, never cumulative: each block is scanned by its own value, so
// what one header of a message costs is not charged to the next.
//
// # Precedence
//
// Several budgets can be exceeded by the same byte — a single enormous line is
// also an enormous field, and eventually an enormous block. The one that is
// reported is the one bounding the smallest scope, in this order: the physical
// line, the logical field, the whole block, and then the field count, which can
// only be exceeded once a field has been read whole. Deciding it here, once,
// is what makes the choice independent of the order the bytes happen to arrive
// in.
type headerScan struct {
	// lim is the configuration this block is read under. It is never modified.
	lim HeaderLimits

	// consumed is the wire bytes of this block read out of the input so far. It
	// counts whether or not a budget is on, because it is what every offset
	// below is measured with.
	consumed int64

	// fieldStart is consumed as it stood when the current logical field began,
	// and fieldBytes is that field's wire bytes so far, its line terminators
	// included.
	fieldStart int64
	fieldBytes int64

	// inField reports whether the line being read belongs to a logical field,
	// which is what decides whether MaxFieldBytes is charged for it at all.
	//
	// Not every line of a block is part of a field. The empty line that ends the
	// block is a line of the block — MaxBytes counts its bytes, and MaxLineBytes
	// sees a line whose content is empty — but it is not a field and no field's
	// budget may be spent on it. Charging it would make the smallest field budget
	// refuse a block for a field it does not contain: a block that is nothing but
	// that empty line has no field in it, and must be read under any budget at
	// all.
	//
	// It is settled before the line's bytes are charged rather than after the
	// line has been read; see readContinuedLineSlice.
	inField bool

	// lineStart is consumed as it stood when the current physical line began,
	// and lineBytes is that line's bytes so far, not counting the terminator
	// that will end it.
	lineStart int64
	lineBytes int64

	// pendingCR records a '\r' at the end of the bytes read so far that is not
	// yet known to be data: it is the first byte of a CRLF terminator when the
	// next byte is '\n', and an ordinary byte of the line otherwise. It is
	// charged to lineBytes as soon as the next read decides which it was, so
	// that a terminator split across two reads is never counted as content and
	// never miscounted as one byte short.
	pendingCR bool

	// count is the logical field candidates charged to this block so far.
	count int64

	// refusal is set once a budget has refused this block, and is what the
	// callers of readHeader turn into a HeaderLimitError. Only the first is
	// kept: a block is refused once.
	refusal *headerRefusal

	// base is where this block begins in its reader's input, which a refusal
	// of the shared budget is built with; see HeaderBudget.
	base int64

	// shared is the refusal of the shared budget that refused this block, which
	// the callers of readHeader return as it is rather than building another.
	shared *HeaderLimitError
}

// refuseShared records the shared budget's refusal e as this block's.
func (s *headerScan) refuseShared(e *HeaderLimitError) error {
	if s.refusal == nil {
		s.shared = e
	}
	return s.refuse(e.Kind(), e.Limit(), e.UnitOffset()-s.base)
}

// limitError returns the error a refused block is reported with: the shared
// budget's own refusal when that is what refused it, and otherwise a new
// HeaderLimitError for the block that began at s.base.
func (s *headerScan) limitError() *HeaderLimitError {
	if s.shared != nil {
		return s.shared
	}
	ref := s.refusal
	return newHeaderLimitError(ref.kind, ref.limit, s.base, s.base+ref.offset)
}

// refuse records the first refusal of this block and returns
// errBudgetExhausted, which is the error every budget of this scan stops with.
func (s *headerScan) refuse(kind HeaderLimitKind, limit, offset int64) error {
	if s.refusal == nil {
		s.refusal = &headerRefusal{kind: kind, limit: limit, offset: offset}
	}
	return errBudgetExhausted
}

// beginField marks the start of a logical header field. Every line read until
// the next beginField or beginBlockEnd belongs to it, which is its first
// physical line and every continuation line that follows.
func (s *headerScan) beginField() {
	s.fieldStart = s.consumed
	s.fieldBytes = 0
	s.inField = true
}

// beginBlockEnd marks the start of the empty line that ends the block.
//
// That line is read outside any field, so MaxFieldBytes is not charged for it
// and cannot refuse it. The block's own budget still counts its bytes, and the
// line budget still sees it — as a line whose content is empty, its terminator
// excluded like any other line's.
func (s *headerScan) beginBlockEnd() {
	s.inField = false
}

// beginLine marks the start of a physical line.
func (s *headerScan) beginLine() {
	s.lineStart = s.consumed
	s.lineBytes = 0
	s.pendingCR = false
}

// chargeField charges one logical field candidate to the field count.
//
// It is charged for every candidate the block yields, including one that is
// skipped afterwards for having an empty name: a budget that only counted the
// fields that survived parsing would be evaded by a block of nothing but
// colons. Continuation lines are not candidates — they belong to the field they
// continue — so a field is charged once however many lines it spans.
func (s *headerScan) chargeField() error {
	s.count++
	if s.lim.MaxCount > 0 && s.count > s.lim.MaxCount {
		return s.refuse(HeaderLimitCount, s.lim.MaxCount, s.fieldStart)
	}
	if t := s.lim.Total; t != nil {
		if e := t.chargeField(s.base, s.fieldStart); e != nil {
			return s.refuseShared(e)
		}
	}
	return nil
}

// preflightField refuses the field candidate about to begin, before any of it
// is read, when the shared budget has no field left for it or has already
// refused. The caller has established that a candidate exists: a byte is there
// and it does not begin the empty line that ends the block.
//
// It comes before every budget of the candidate itself, none of which has
// seen a byte of it yet. The per-block field count keeps its own timing and is
// charged with the shared one once the field has been read; see HeaderBudget.
func (s *headerScan) preflightField() error {
	t := s.lim.Total
	if t == nil {
		return nil
	}
	if e := t.preflightField(s.base, s.consumed); e != nil {
		return s.refuseShared(e)
	}
	return nil
}

// consume charges the bytes just read out of the input by one ReadSlice, whose
// result was l and err, to every budget of this block.
//
// It is called before those bytes are kept, so that nothing beyond a budget is
// ever accumulated: what a refused line or field appends is at most one of the
// reader's buffers past the budget, and a refused block keeps nothing at all.
//
// No sum of a budget and a usage is ever computed. Each charge is compared
// against the headroom that remains instead, which is exact for every value in
// range, so a budget of math.MaxInt64 is an ordinary finite budget here rather
// than one that wraps into an accepted one.
func (s *headerScan) consume(l []byte, err error) error {
	data, ended := s.classify(l, err)

	// The physical line, first: it bounds the smallest scope.
	if s.lim.MaxLineBytes > 0 {
		if data > s.lim.MaxLineBytes-s.lineBytes {
			return s.refuse(HeaderLimitLineBytes, s.lim.MaxLineBytes, s.lineStart)
		}
	}
	// The logical field counts wire bytes, terminators included, so it is
	// charged what was read rather than what the line kept. A line that belongs
	// to no field — the empty line that ends the block — is charged to no field
	// and can refuse none; see inField.
	n := int64(len(l))
	if s.inField && s.lim.MaxFieldBytes > 0 {
		if n > s.lim.MaxFieldBytes-s.fieldBytes {
			return s.refuse(HeaderLimitFieldBytes, s.lim.MaxFieldBytes, s.fieldStart)
		}
	}
	if s.lim.MaxBytes > 0 {
		if n > s.lim.MaxBytes-s.consumed {
			return s.refuse(HeaderLimitBytes, s.lim.MaxBytes, 0)
		}
	}
	// The shared budget last: it bounds the widest scope, and charging it is
	// the commit, so nothing may refuse these bytes after it has admitted them.
	if t := s.lim.Total; t != nil {
		if e := t.chargeBytes(n, s.base, s.consumed); e != nil {
			return s.refuseShared(e)
		}
	}

	s.lineBytes += data
	if s.inField {
		s.fieldBytes += n
	}
	s.consumed += n
	if ended {
		s.pendingCR = false
	}
	return nil
}

// classify reports how many of the bytes just read belong to the content of the
// current physical line, and whether that line ended.
//
// A terminator is not content. Telling one apart takes a little care, because
// ReadSlice stops at '\n' or at a full buffer, so a "\r\n" can arrive as a '\r'
// at the end of one read and a '\n' at the start of the next — and the same
// '\r' is an ordinary byte of the line when what follows it is not '\n', or
// when nothing follows it at all. The '\r' is therefore held back by pendingCR
// and charged as soon as the next read settles what it was.
func (s *headerScan) classify(l []byte, err error) (data int64, ended bool) {
	n := int64(len(l))

	if s.pendingCR {
		if len(l) > 0 && l[0] == '\n' {
			// The held-back '\r' was the first byte of this terminator.
			s.pendingCR = false
		} else {
			// It was content after all.
			data++
			s.pendingCR = false
		}
	}

	switch {
	case err == nil:
		// l ends with the '\n' that terminated this line.
		n--
		if len(l) >= 2 && l[len(l)-2] == '\r' {
			n--
		}
		return data + n, true

	case err == bufio.ErrBufferFull:
		// No terminator yet. A trailing '\r' may be the start of one, so it is
		// held back rather than charged.
		if len(l) > 0 && l[len(l)-1] == '\r' {
			s.pendingCR = true
			n--
		}
		return data + n, false

	default:
		// The input stopped. Nothing here is a terminator, including a trailing
		// '\r' and any '\r' that was held back.
		return data + n, true
	}
}

// readLineSlice appends the next physical line of r to line, without its
// terminator, and charges every byte it consumes to scan.
//
// It reads through ReadSlice rather than bufio.Reader.ReadLine because a budget
// has to be charged what was actually consumed, and ReadLine reports a line
// whose terminator it has already stripped — leaving no way to tell a line
// ended by "\n" from one ended by "\r\n" or by the end of the input. Reading
// the raw bytes and stripping the terminator from the accumulated result is
// what ReadLine does internally, so what this returns is what it returned: the
// same bytes for a "\r\n" that straddles the buffer, and the same last line for
// an input that ends without a terminator at all.
func readLineSlice(r *bufio.Reader, line []byte, scan *headerScan) ([]byte, error) {
	// appended bounds how much of line belongs to this call, so that the
	// terminator stripped below cannot reach into continuation lines a caller
	// accumulated before it.
	appended := 0

	scan.beginLine()

	for {
		l, err := r.ReadSlice('\n')

		// The budgets are charged before the bytes are kept, so a line longer
		// than a budget is refused after one buffer rather than accumulated:
		// what this appends never exceeds the budget by more than that buffer.
		if berr := scan.consume(l, err); berr != nil {
			return line, berr
		}

		if err == bufio.ErrBufferFull {
			// The line is longer than r's buffer; keep going.
			line = append(line, l...)
			appended += len(l)
			continue
		}

		if err != nil {
			if len(l) == 0 {
				return line, err
			}
			// ReadLine reports the bytes it did read and leaves the error for
			// the next read, and so does this. There is no terminator to strip:
			// ReadSlice only stops without one at the end of the input.
			return append(line, l...), nil
		}

		line = append(line, l...)
		appended += len(l)

		// err is nil, so l ends with the '\n' that terminated this line.
		drop := 1
		if appended > 1 && line[len(line)-2] == '\r' {
			drop = 2
		}
		return line[:len(line)-drop], nil
	}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t'
}

func validHeaderKeyByte(b byte) bool {
	c := int(b)
	return c >= 33 && c <= 126 && c != ':'
}

// trim returns s with leading and trailing spaces and tabs removed.
// It does not assume Unicode or UTF-8.
func trim(s []byte) []byte {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	n := len(s)
	for n > i && isSpace(s[n-1]) {
		n--
	}
	return s[i:n]
}

func hasContinuationLine(r *bufio.Reader) bool {
	c, err := r.ReadByte()
	if err != nil {
		return false // bufio will keep err until next read.
	}
	r.UnreadByte()
	return isSpace(c)
}

// atBlockEnd reports whether the next bytes of r are the empty line that ends a
// header block, without consuming any of them.
//
// It is what lets a field budget be charged only for the lines of an actual
// field. The question has to be settled before those bytes are charged rather
// than after the line has been read: charging first and refunding on discovering
// an empty line would already have refused the block, and a refusal is what this
// package returns rather than something it can take back. Reading the line first
// and deciding afterwards is equally unavailable, because a field longer than a
// budget must be refused while it is read and not once it is whole.
//
// At most two bytes decide it, and neither is consumed. A '\n' ends an empty
// line on its own; a '\r' ends one when a '\n' follows it. Anything else is the
// first byte of a field's first line, including a '\r' that the input never
// follows with anything, which is content and is read as content.
//
// Nothing is read ahead here that the parser was not about to read anyway:
// these are the first bytes of the line it goes on to read either way, and Peek
// leaves every one of them for it. The same look-ahead already happens one line
// further on, in hasContinuationLine.
//
// candidate reports the other outcome of the same look: a byte is there and it
// does not begin that empty line, so a field candidate begins here. An input
// with no byte left — its end, or a failed read — is neither.
func atBlockEnd(r *bufio.Reader) (end, candidate bool) {
	b, err := r.Peek(1)
	if err != nil || len(b) == 0 {
		return false, false
	}
	switch b[0] {
	case '\n':
		return true, false
	case '\r':
		b, err := r.Peek(2)
		if err == nil && len(b) == 2 && b[1] == '\n' {
			return true, false
		}
	}
	return false, true
}

func readContinuedLineSlice(r *bufio.Reader, scan *headerScan) ([]byte, error) {
	// The empty line that ends the block is a line of the block but not a
	// logical field, so it is read outside any field's budget. Settling that
	// here — from bytes that are looked at and not consumed — is what keeps a
	// field budget from being spent on a field that is not there.
	end, candidate := atBlockEnd(r)
	if end {
		scan.beginBlockEnd()
		return readLineSlice(r, nil, scan)
	}

	// A candidate that the shared field budget has no room for is refused here,
	// from the byte that proves it exists, rather than after it has been read
	// whole: no per-block budget may be in force to bound how long it is.
	if candidate {
		if err := scan.preflightField(); err != nil {
			return nil, err
		}
	}

	// A logical field begins here and ends where this call does, so its budget
	// covers this line and every continuation line read below.
	scan.beginField()

	// Read the first line. We preallocate slice that it enough
	// for most fields.
	line, err := readLineSlice(r, make([]byte, 0, 256), scan)
	if err == io.EOF && len(line) == 0 {
		// Header without a body
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	if len(line) == 0 { // blank line - no continuation
		return line, nil
	}

	line = append(line, '\r', '\n')

	// Read continuation lines.
	for hasContinuationLine(r) {
		line, err = readLineSlice(r, line, scan)
		if err != nil {
			if isBudgetExhausted(err) {
				// The budget is a hard stop rather than a read that can be
				// retried: the bytes it refused are already gone, so the field
				// is not handed back half read. A read failure is not such a
				// stop, however it presents itself, and keeps the long-standing
				// behavior below.
				return nil, err
			}
			break // bufio will keep err until next read.
		}

		line = append(line, '\r', '\n')
	}

	return line, nil
}

func writeContinued(b *strings.Builder, l []byte) {
	// Strip trailing \r, if any
	if len(l) > 0 && l[len(l)-1] == '\r' {
		l = l[:len(l)-1]
	}
	l = trim(l)
	if len(l) == 0 {
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.Write(l)
}

// Strip newlines and spaces around newlines.
func trimAroundNewlines(v []byte) string {
	var b strings.Builder
	b.Grow(len(v))
	for {
		i := bytes.IndexByte(v, '\n')
		if i < 0 {
			writeContinued(&b, v)
			break
		}
		writeContinued(&b, v[:i])
		v = v[i+1:]
	}

	return b.String()
}

// ReadHeader reads a MIME header from r. The header is a sequence of possibly
// continued "Key: Value" lines ending in a blank line.
//
// To avoid denial of service attacks, the provided bufio.Reader should be
// reading from an io.LimitedReader or a similar Reader to bound the size of
// headers, or the header should be read with ReadHeaderLimit or
// ReadHeaderLimits, which bound the block itself — its bytes, its fields, its
// lines — and leave what follows it alone.
func ReadHeader(r *bufio.Reader) (Header, error) {
	return readHeader(r, &headerScan{})
}

// ReadHeaderLimit reads a MIME header from r like ReadHeader, but refuses one
// whose wire bytes exceed maxBytes.
//
// What is counted is the header block exactly as it appears in the input, from
// the first byte of its first field through the empty line that ends it, that
// empty line included. A block of exactly maxBytes bytes is read normally; one
// byte more and ReadHeaderLimit returns a *HeaderLimitError, which verifies
// errors.Is(err, ErrHeaderTooBig). A maxBytes of zero or below is no budget at
// all, which is what ReadHeader reads under.
//
// A refused block yields no header at all — not the fields that happened to fit
// before the budget ran out, which no caller could tell apart from a message
// that simply did not have the rest.
//
// The refusal is this package's own, and can be told apart as such: test it
// with HeaderLimit, which reports only a refusal this package made. An r whose
// underlying reader fails with ErrHeaderTooBig, with something wrapping it, or
// with a HeaderLimitError value of its own, is an ordinary read failure and is
// returned as one, on the terms ReadHeader returns any read failure.
//
// The budget is checked while the block is read, not after it has been built:
// at most one of r's buffers is read past the budget, and no more than maxBytes
// of the block is ever retained, so a header that does not end cannot cost
// unbounded memory. Taking the budget here rather than bounding r's own source
// is what keeps it a budget on the header: a bounded source also shortens the
// reads that follow the block, and would take the body's bytes with it.
//
// A block that overran its budget leaves r where the failure happened. Some of
// what follows the header has already been consumed, and nothing here can say
// how much of the input the block would have covered, so r cannot be
// resynchronized and must not be read on as though it could.
func ReadHeaderLimit(r *bufio.Reader, maxBytes int64) (Header, error) {
	return ReadHeaderLimits(r, HeaderLimits{MaxBytes: maxBytes})
}

// ReadHeaderLimits reads a MIME header from r like ReadHeader, but refuses one
// that exceeds any budget of limits.
//
// It is the general form of ReadHeaderLimit, which is the same call with only
// HeaderLimits.MaxBytes set. Everything ReadHeaderLimit documents holds here for
// every budget: what is counted is the block exactly as it appears in the
// input; a block at exactly a budget is read normally and one unit more is
// refused; a budget of zero or below is no budget at all; a refused block yields
// no header at all rather than the fields that happened to fit; the budgets are
// checked while the block is read rather than after it has been built, so no
// more than one of r's buffers past a budget is ever read and nothing beyond a
// budget is retained; and a block that overran leaves r where the failure
// happened, unsynchronized, not to be read on as though it could be.
//
// What each budget counts is stated on HeaderLimits, and which one a refusal
// reports when several were exceeded at once is stated on headerScan: the
// smallest scope wins. The refusal is a *HeaderLimitError whose Kind names that
// budget and whose UnitOffset says where in the input the line or field it
// refused began; it verifies errors.Is against the sentinel of its kind. Test
// it with HeaderLimit, which reports only a refusal this package made, so that
// an r whose underlying reader fails with one of those sentinels, or hands back
// a HeaderLimitError value of its own, stays the ordinary read failure it is.
//
// limits.Total differs from the per-block budgets in three ways; see
// HeaderBudget. It is cumulative: the block is charged to it on top of what
// earlier blocks read with the same budget spent. It is checked after the
// per-block byte budgets, which win a read that exceeds both, and a spent field
// count refuses the next field candidate at its first byte, before any of it
// is read. And its refusal is not built for this call: it is the budget's one
// refusal, the identical value Refusal returns, whose offsets are in the input
// of whichever reader made it.
func ReadHeaderLimits(r *bufio.Reader, limits HeaderLimits) (Header, error) {
	scan := &headerScan{lim: limits}
	h, err := readHeader(r, scan)
	if isBudgetExhausted(err) {
		// The block began where r stood when this was called, so that is the
		// offset reported, on the same terms Part.HeaderOffset uses: relative
		// to the input the reader was given. The refused unit's own offset is
		// measured from there, and is a position in that same input.
		return h, scan.limitError()
	}
	return h, err
}

// headerResult returns what readHeader returns for a block that ended with err:
// the fields it managed to read, or none of them when the budget refused the
// block.
//
// A refused block is all or nothing, because a partial one cannot be told apart
// from a whole one. A caller handed the fields that happened to fit before the
// budget ran out has a Header that answers every question — Get, Has, Len —
// as though the message simply did not have the rest, which is a different and
// far more dangerous claim than "this header was not read".
//
// Nothing else changes. Every other way a block can end mid-way — a malformed
// line, a read failure, an input that stops — keeps returning what it read,
// which is what those paths have always done and what callers of ReadHeader
// already rely on.
func headerResult(fs []*headerField, err error) (Header, error) {
	if isBudgetExhausted(err) {
		return newHeader(nil), err
	}
	return newHeader(fs), err
}

func readHeader(r *bufio.Reader, scan *headerScan) (Header, error) {
	fs := make([]*headerField, 0, 32)

	// The first line cannot start with a leading space.
	if buf, err := r.Peek(1); err == nil && isSpace(buf[0]) {
		scan.beginField()
		line, err := readLineSlice(r, nil, scan)
		if err != nil {
			return headerResult(fs, err)
		}

		return headerResult(fs, fmt.Errorf("message: malformed MIME header initial line: %v", string(line)))
	}

	for {
		kv, err := readContinuedLineSlice(r, scan)
		if len(kv) == 0 {
			return headerResult(fs, err)
		}

		// One logical field candidate has been read, so it is charged to the
		// count now — before anything about its syntax is looked at, and
		// therefore before anything about its syntax could excuse it from the
		// budget. A candidate whose name turns out to be empty is skipped below
		// but stays charged here, so that a block of nothing but colons cannot
		// buy more candidates than a block of ordinary fields.
		//
		// readContinuedLineSlice returned one whole logical field, continuation
		// lines included, so this charges once however many physical lines the
		// field spans.
		if cerr := scan.chargeField(); cerr != nil {
			return headerResult(fs, cerr)
		}

		// Key ends at first colon; should not have trailing spaces but they
		// appear in the wild, violating specs, so we remove them if present.
		i := bytes.IndexByte(kv, ':')
		if i < 0 {
			// Some malformed messages use a semicolon instead of a colon as
			// the field separator; accept it as a fallback.
			i = bytes.IndexByte(kv, ';')
			if i < 0 {
				return headerResult(fs, fmt.Errorf("message: malformed MIME header line: %v", string(kv)))
			}
		}

		keyBytes := trim(kv[:i])

		// Verify that there are no invalid characters in the header key.
		// See RFC 5322 Section 2.2
		for _, c := range keyBytes {
			if !validHeaderKeyByte(c) {
				return headerResult(fs, fmt.Errorf("message: malformed MIME header key: %v", string(keyBytes)))
			}
		}

		key := textproto.CanonicalMIMEHeaderKey(string(keyBytes))

		// As per RFC 7230 field-name is a token, tokens consist of one or more
		// chars. We could return a an error here, but better to be liberal in
		// what we accept, so if we get an empty key, skip it.
		if key == "" {
			continue
		}

		i++ // skip colon or semicolon
		v := kv[i:]

		value := trimAroundNewlines(v)
		fs = append(fs, newHeaderField(key, value, kv))

		if err != nil {
			return headerResult(fs, err)
		}
	}
}

func foldLine(v string, maxlen int) (line, next string, ok bool) {
	ok = true

	if maxlen < 1 {
		// A header key longer than maxHeaderLen drives maxlen non-positive, which
		// would make foldBefore a negative slice bound below. Clamp so pathological
		// input folds (ugly but valid) instead of panicking.
		maxlen = 1
	}

	// We'll need to fold before maxlen
	foldBefore := maxlen + 1
	foldAt := len(v)

	var folding string
	if foldBefore > len(v) {
		// We reached the end of the string
		if v[len(v)-1] != '\n' {
			// If there isn't already a trailing CRLF, insert one
			folding = "\r\n"
		}
	} else {
		// Find the closest whitespace before maxlen
		foldAt = strings.LastIndexAny(v[:foldBefore], " \t\n")

		if foldAt == 0 {
			// The whitespace we found was the previous folding WSP
			foldAt = foldBefore - 1
		} else if foldAt < 0 {
			// We didn't find any whitespace, we have to insert one
			foldAt = foldBefore - 2
		}

		switch v[foldAt] {
		case ' ', '\t':
			if v[foldAt-1] != '\n' {
				folding = "\r\n" // The next char will be a WSP, don't need to insert one
			}
		case '\n':
			folding = "" // There is already a CRLF, nothing to do
		default:
			// Another char, we need to insert CRLF + WSP. This will insert an
			// extra space in the string, so this should be avoided if
			// possible.
			folding = "\r\n "
			ok = false
		}
	}

	return v[:foldAt] + folding, v[foldAt:], ok
}

const (
	preferredHeaderLen = 76
	maxHeaderLen       = 998
)

// formatHeaderField formats a header field, ensuring each line is no longer
// than 76 characters. It tries to fold lines at whitespace characters if
// possible. If the header contains a word longer than this limit, it will be
// split.
func formatHeaderField(k, v string) string {
	s := k + ": "

	if v == "" {
		return s + "\r\n"
	}

	first := true
	for len(v) > 0 {
		// If this is the first line, substract the length of the key
		keylen := 0
		if first {
			keylen = len(s)
		}

		var l, next string
		if keylen < preferredHeaderLen {
			// First try with a soft limit
			var ok bool
			l, next, ok = foldLine(v, preferredHeaderLen-keylen)
			if !ok {
				// Folding failed to preserve the original header field value. Try
				// with a larger, hard limit.
				l, next, _ = foldLine(v, maxHeaderLen-keylen)
			}
		} else {
			// The key alone already exceeds the soft limit, so a soft fold would
			// compute a negative width. Fold directly at the hard limit.
			l, next, _ = foldLine(v, maxHeaderLen-keylen)
		}
		v = next
		s += l
		first = false
	}

	return s
}

// WriteHeader writes a MIME header to w.
func WriteHeader(w io.Writer, h Header) error {
	for i := len(h.l) - 1; i >= 0; i-- {
		f := h.l[i]
		if rawField, err := f.raw(); err == nil {
			if _, err := w.Write(rawField); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("failed to write header field #%v (%q): %w", len(h.l)-i, f.k, err)
		}
	}

	_, err := w.Write([]byte{'\r', '\n'})
	return err
}
