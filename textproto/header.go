package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/textproto"
	"sort"
	"strings"
)

type headerField struct {
	b []byte // Raw header field, including whitespace

	k string
	v string

	// seq is where this field stood among the fields of the block it was
	// read from, counting from 1, or 0 for a field that was not read from a
	// block (Add, AddRaw, Set, HeaderFromMap). It is never modified, so the
	// copies of a Header that share this field agree on it. WriteHeader writes
	// a header's orphan records in their place among the fields read with
	// them by it; see OrphanContinuation.
	seq int
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
// The header representation is idempotent for a well-formed block: a complete
// block whose every field was read as a field and kept, and whose every line,
// the empty line that ends it included, ends with CRLF — with or without a
// UTF-8 byte order mark before it — is written back unchanged exactly as it
// was read, whitespace and field order included, as long as its fields are not
// modified. This is required for e.g. DKIM. Anything else is not a replay of
// the input: lines ended otherwise, and a block io.EOF ended, are written with
// CRLF and the empty line; a line the reader skipped or repaired rather than
// kept as a field — such as one whose name is empty — is not written at all
// (see ReadHeader, WriteHeader and ByteOrderMark). A block that began with a
// UTF-16 or UTF-32 mark is not read, and is not written. A block whose first
// lines began with white space keeps those lines apart, as its malformed prefix,
// and writes them back first, before every field (see MalformedPrefix). A
// damaged line that continuation lines followed keeps them, with it, apart as
// an orphan record, written back in its place among the fields (see
// OrphanContinuation); a damaged line with no continuation line is not.
//
// Mutating the header is restricted: the only two allowed operations are
// inserting a new header field at the top and deleting a header field. This is
// again necessary for DKIM.
type Header struct {
	// Fields are in reverse order so that inserting a new field at the top is
	// cheap.
	l []*headerField
	m map[string][]*headerField

	// ends is the line endings of the block this header was read from, or nil
	// when it was not read whole by this package or has been changed since. It
	// is never modified once set, so a Copy shares it; every mutation drops it.
	// See LineEndings.
	ends *LineEndings

	// bom is the byte order mark the block this header was read from began
	// with. Unlike ends it is the header's own: Copy and every mutation keep
	// it. See ByteOrderMark.
	bom ByteOrderMark

	// prefix is the malformed prefix the block this header was read from began
	// with, or nil. Like bom it is the header's own and is never modified once
	// set: Copy shares it and every mutation keeps it. See MalformedPrefix.
	prefix *malformedPrefix

	// orphans is the orphan records of the block this header was read from,
	// in the order they were read, or nil. Like prefix it is the header's own
	// and is never modified once set — its length is its capacity, so nothing
	// appends to it in place: Copy shares it and every mutation keeps it. See
	// OrphanContinuation.
	orphans []*orphanRecord

	// heal is what recovery did to the block, or nil for a block that needed
	// none or was not read with HeaderLimits.Recover. See recover.go.
	heal *headerHeal
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

	h.ends = nil
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

	h.ends = nil
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

	h.ends = nil
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
	return Header{l: l, m: m, ends: h.ends, bom: h.bom, prefix: h.prefix, orphans: h.orphans, heal: h.heal}
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

	fs.h.ends = nil
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

	fs.h.ends = nil
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

	// ends is the line endings of this block so far; see LineEndings.
	ends LineEndings

	// crKey is set when the physical line just read ended with a bare '\r'
	// whose next byte can begin a field name, which is the one place a bare
	// '\r' is not yet known to be a line ending; see the stray carriage return
	// rule on ReadHeader. probePrev is crKey taken up by the candidate that
	// follows, while its first line is probed.
	crKey, probePrev crLine

	// probe is the state of that probe: off, or how much of "name *WSP :" the
	// candidate's first line has shown so far.
	probe probeState

	// lineRejoined reports that the line being read is a probed candidate whose
	// '\r' turned out to be content, so that it continues the previous line.
	// rejoined reports the same for the logical field readContinuedLineSlice
	// returns, and dropPrev that a failure cut such a line, or one whose '\r'
	// it left undecided, so the previous line is cut with it.
	lineRejoined, rejoined, dropPrev bool

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

	// fail is the first error a read of this block returned, io.EOF included,
	// or nil. bufio.Reader hands a read error out once and then forgets it, so
	// the scanner keeps it: once it is set no read of the block refills the
	// reader, and only bytes it already holds are read. See peek and nextChunk.
	fail error

	// capturing reports that the block began with a malformed prefix that may
	// still grow, and capture is that prefix's exact bytes so far, line endings
	// as they were read, followed by those of whatever is being read after it
	// while it is not yet known whether that continues it. capLast is where the
	// last physical line kept whole begins in capture. Bytes are captured only
	// after the charge that admitted them, so capture never holds more than the
	// budgets admitted; nothing is charged for capturing them. See prefix.go.
	capturing bool
	capture   []byte
	capLast   int

	// parent is the multipart reader whose part header this block is, or nil.
	// It is looked at only after a malformed prefix or an orphan record, to
	// end the block at that reader's next delimiter line rather than read the
	// line as a field, and after a damaged line whose '\r' is still to be
	// settled; atDelim reports that the block ended there. See prefix.go and
	// orphan.go.
	parent  *MultipartReader
	atDelim bool

	// replay is bytes taken out of the reader to decide something and then
	// read as the block's: the beginning of a line that looked like such a
	// delimiter and proved not to be one (holdDelimiter), which holds no '\r'
	// or '\n', or the lines a '\r' that is content joins to a damaged line,
	// with their line endings and the first byte of the continuation line that
	// proved them a record (settleCR). They are the next bytes of the input:
	// nextChunk returns them before reading on, and hasContinuationLine and
	// atBlockEnd look at them first (see vpeek). They were not charged when
	// they were taken out; they are charged when they are read, as any bytes
	// of a line are.
	replay []byte

	// vbuf is the room vpeek joins the end of replay and the reader's next
	// bytes in.
	vbuf [2]byte

	// terms is how each physical line of the logical candidate being read
	// ended, in order: the first termsFix entries in termsFix, the rest in
	// termsMore. It is what turns a candidate's lines, which are kept ended by
	// "\r\n" whatever ended them, back into the input's exact bytes when the
	// candidate proves to be an orphan record (see orphan.go). It is one byte
	// a line, in storage the scan owns, reused by every candidate of the
	// block: a candidate of up to len(termsFix) lines allocates nothing for
	// it, and a longer one grows termsMore, which the next keeps.
	termsN    int
	termsFix  [16]lineTerm
	termsMore []lineTerm

	// unread is, under HeaderLimits.Recover, the bytes of a damaged line read
	// out of the reader that began the body: the block ended before it, and
	// the caller gives them back to what reads the body. heal is what recovery
	// did to the block. See recover.go.
	unread []byte
	heal   headerHeal
}

// A crLine is what a line ended by a bare '\r' that may still prove to be
// content left behind: where that line and its field began, what they had been
// charged, and where the '\r' was.
type crLine struct {
	ok                     bool
	lineStart, lineBytes   int64
	fieldStart, fieldBytes int64
	cr                     int64
}

// probeState is how far a probed candidate's first line has matched
// 1*name-byte *WSP ":".
type probeState uint8

const (
	probeOff   probeState = iota
	probeName             // name bytes so far
	probeSpace            // name bytes, then white space
)

// peek is r.Peek(n) for the scanner. It records the first error a peek
// returns, and once a read of the block has failed it looks only at bytes r
// already holds, returning those with the failure when they are fewer than n.
//
// The scanner never peeks more than r's buffer holds — at most two bytes past
// what is buffered, or one more byte than is buffered while that is less than
// the whole buffer, and a bufio.Reader holds at least 16 — so Peek's own
// bufio.ErrBufferFull cannot occur here: that value is the input's own failure
// and is recorded like any other. It is never mistaken for a line longer than
// the buffer, and never read on without bytes.
func (s *headerScan) peek(r *bufio.Reader, n int) ([]byte, error) {
	if s.fail == nil {
		b, err := r.Peek(n)
		if err != nil {
			s.fail = err
		}
		return b, err
	}
	if avail := r.Buffered(); avail < n {
		// Peeking no more than is buffered never refills.
		b, _ := r.Peek(avail)
		return b, s.fail
	}
	return r.Peek(n)
}

// lineEnd is how the bytes nextChunk returned end.
type lineEnd uint8

const (
	// endNone: the line goes on past these bytes, all of them content.
	endNone lineEnd = iota
	// endLF, endCRLF: the bytes end with that line ending.
	endLF
	endCRLF
	// endCR: the bytes end with a '\r' not followed by '\n'. Whether it ends
	// the line is the caller's to decide from what follows it, which nextChunk
	// reports and does not consume.
	endCR
	// endInput: the input stopped after these bytes, with io.EOF or a failure.
	endInput
)

// eofNext is what nextChunk reports as the byte after a '\r' that io.EOF
// followed.
const eofNext = -1

// nextChunk reads the next bytes of the current physical line out of r, on
// the terms of peek: the first failure is recorded, and once a read of the
// block has failed only what r already holds is read.
//
// What it returns ends at the first line ending, or at a full buffer, or where
// the input stopped, whichever comes first — the bytes ReadSlice('\n') would
// return, except that a '\r' not followed by '\n' also ends them. Which bytes
// come back therefore depends on the input and on r's buffer size alone, not
// on how the input chunks its reads.
//
// A '\r' is never consumed before the byte after it has been seen, because
// that byte is what says whether it is the first byte of a CRLF. When the '\r'
// is the last byte r holds, the bytes before it are returned as content of a
// line that goes on, and the next call looks one byte further, which a
// bufio.Reader of at least 16 bytes always can. That is the scanner's whole
// look-ahead: one byte past a '\r'. With endCR, next is that byte, or eofNext
// when io.EOF followed the '\r'. A '\r' that any other failure followed is
// returned with endInput and the failure, as the last byte of a line the
// failure cut.
func (s *headerScan) nextChunk(r *bufio.Reader) (b []byte, end lineEnd, next int, err error) {
	// The beginning of a line taken out of the reader to be decided comes
	// first, as content of a line that goes on; see holdDelimiter.
	// It is handed out a buffer at a time, as the reader would have.
	// Bytes settleCR took out of the reader for a record are the next bytes
	// of the input too, line endings included, and are read on the same
	// terms as the reader's: content a buffer at a time, a line ending as
	// soon as it is whole, and a '\r' they end with decided by the reader's
	// next byte.
	if len(s.replay) > 0 {
		rp := s.replay
		i := bytes.IndexAny(rp, "\r\n")
		take := func(n int) []byte {
			b := rp[:n]
			s.replay = rp[n:]
			if len(s.replay) == 0 {
				s.replay = nil
			}
			return b
		}
		switch {
		case i < 0 || i >= r.Size():
			return take(min(len(rp), r.Size())), endNone, 0, nil
		case rp[i] == '\n':
			return take(i + 1), endLF, 0, nil
		case i+1 < len(rp) && rp[i+1] == '\n':
			return take(i + 2), endCRLF, 0, nil
		case i+1 < len(rp):
			return take(i + 1), endCR, int(rp[i+1]), nil
		case i > 0:
			// The '\r' is the last byte held: the bytes before it are
			// content either way, and the next call decides it.
			return take(i), endNone, 0, nil
		}
		// The replay is that '\r' alone: the reader's next byte decides it.
		nb, perr := s.peek(r, 1)
		switch {
		case len(nb) == 1 && nb[0] == '\n':
			take(1)
			r.Discard(1)
			return crlf, endCRLF, 0, nil
		case len(nb) == 1:
			return take(1), endCR, int(nb[0]), nil
		case perr == io.EOF:
			return take(1), endCR, eofNext, nil
		}
		return take(1), endInput, 0, perr
	}

	// scanned is how many held bytes are known to hold neither '\r' nor '\n',
	// so that holding on for more bytes never searches the same ones again.
	scanned := 0
	for {
		n := r.Buffered()
		if n == 0 {
			if _, err := s.peek(r, 1); err != nil {
				return nil, endInput, 0, err
			}
			continue
		}
		w, _ := r.Peek(n)
		i := bytes.IndexAny(w[scanned:], "\r\n")
		if i >= 0 {
			i += scanned
		} else {
			scanned = n
		}
		switch {
		case i >= 0 && w[i] == '\n':
			// No '\r' comes before it, or it would have been found first.
			r.Discard(i + 1)
			return w[:i+1], endLF, 0, nil
		case i >= 0 && i+1 < n:
			if w[i+1] == '\n' {
				r.Discard(i + 2)
				return w[:i+2], endCRLF, 0, nil
			}
			r.Discard(i + 1)
			return w[:i+1], endCR, int(w[i+1]), nil
		}

		// Nothing held decides where the line ends. Hold on, as ReadSlice
		// would, until something does or the buffer is full. The peek is for one
		// byte more than is held, which is at most the whole buffer.
		if s.fail == nil && n < r.Size() {
			if i >= 0 {
				// The '\r' that is the last byte held is where to look again.
				scanned = i
			}
			s.peek(r, n+1)
			continue
		}

		// The buffer is full, or the input has stopped.
		if i < 0 {
			r.Discard(n)
			if s.fail != nil {
				return w, endInput, 0, s.fail
			}
			return w, endNone, 0, nil
		}
		// A '\r' is the last byte held: the bytes before it are content
		// either way, and the next call decides the '\r'.
		if i > 0 {
			r.Discard(i)
			return w[:i], endNone, 0, nil
		}
		// The '\r' is all that is held and the input has stopped: a full
		// buffer holds at least 16 bytes, so s.fail is set here.
		r.Discard(1)
		if s.fail == io.EOF {
			return w[:1], endCR, eofNext, nil
		}
		return w[:1], endInput, 0, s.fail
	}
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
	s.lineRejoined = false
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

// charge charges n bytes just read out of the input, data of which are content
// of the current physical line rather than its line ending, to every budget of
// this block.
//
// It is called before those bytes are kept, so that nothing beyond a budget is
// ever accumulated: what a refused line or field appends is at most one of the
// reader's buffers past the budget, and a refused block keeps nothing at all.
// Only bytes read out of the input are charged; the byte nextChunk looked at
// past a '\r' is charged when it is read, as part of whatever it begins.
//
// No sum of a budget and a usage is ever computed. Each charge is compared
// against the headroom that remains instead, which is exact for every value in
// range, so a budget of math.MaxInt64 is an ordinary finite budget here rather
// than one that wraps into an accepted one.
func (s *headerScan) charge(data, n int64) error {
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
	return nil
}

// take charges the content bytes c of the current line, and then term bytes
// of the line ending that follows them, to every budget of this block; ends
// reports that the line ends after them, by that line ending or by io.EOF.
//
// While a candidate's first line is probed (see crLine), c is also matched
// against 1*name-byte *WSP ":". A colon settles that the candidate is a field
// and the '\r' before it ended the previous line: nothing charged changes. A
// byte that cannot continue that match, or the end of the line before any
// colon, settles that the '\r' was content: the bytes before that point are
// charged to the candidate as read, and from there on this line and this field
// are the previous ones continued, charged together; see rejoin. Where that
// happens depends on the bytes alone, so it is the same however they were
// chunked.
func (s *headerScan) take(c []byte, term int64, ends bool) error {
	if s.probe != probeOff {
		k, settled, field := s.probeStep(c)
		if !settled && ends {
			settled, field = true, false
		}
		if settled && field {
			s.probe = probeOff
			s.ends.note(LineEndingCR, s.probePrev.cr)
		} else if settled {
			if err := s.charge(int64(k), int64(k)); err != nil {
				return err
			}
			c = c[k:]
			if err := s.rejoin(); err != nil {
				return err
			}
		}
	}
	return s.charge(int64(len(c)), int64(len(c))+term)
}

// probeStep matches c against what the probed candidate's first line has
// shown so far. It returns the index of the byte that settled the probe, and
// whether that byte made the candidate a field; or, when nothing in c settled
// it, len(c) and settled false.
func (s *headerScan) probeStep(c []byte) (k int, settled, field bool) {
	for k, b := range c {
		switch {
		case b == ':':
			return k, true, true
		case s.probe == probeName && validHeaderKeyByte(b):
		case isSpace(b):
			s.probe = probeSpace
		default:
			return k, true, false
		}
	}
	return len(c), false, false
}

// rejoin makes the probed candidate the continuation of the line the '\r'
// ended: that '\r' is content, so the physical line is the previous one, the
// '\r' and what the candidate has read, and the field is the previous field
// and the candidate together. Both are charged as such from here on, and both
// are checked at once, line first, against what they now hold: a line or field
// the rejoin makes too long is refused here, where it began.
//
// Nothing is refunded. The '\r' was charged to the field and the block when it
// was read; it is now also content of the line. The shared field budget was
// never charged for the candidate, which is charged as a field only once it has
// been read whole as one; see readHeader.
func (s *headerScan) rejoin() error {
	p := s.probePrev
	s.probe = probeOff
	s.lineRejoined = true
	s.rejoined = true

	s.lineStart = p.lineStart
	s.lineBytes = satAdd(satAdd(p.lineBytes, 1), s.lineBytes)
	s.fieldStart = p.fieldStart
	s.fieldBytes = satAdd(p.fieldBytes, s.fieldBytes)

	if s.lim.MaxLineBytes > 0 && s.lineBytes > s.lim.MaxLineBytes {
		return s.refuse(HeaderLimitLineBytes, s.lim.MaxLineBytes, s.lineStart)
	}
	if s.lim.MaxFieldBytes > 0 && s.fieldBytes > s.lim.MaxFieldBytes {
		return s.refuse(HeaderLimitFieldBytes, s.lim.MaxFieldBytes, s.fieldStart)
	}
	return nil
}

// satAdd returns a+b for non-negative a and b, or math.MaxInt64 when that
// would not fit.
func satAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// readLineSlice appends the next physical line of r to line, without its line
// ending, and charges every byte it consumes to scan.
//
// A line ends at "\r\n", at a '\n', or at a '\r' that is not followed by '\n'
// when that '\r' is the first byte of the line, comes right before io.EOF, or
// is followed by white space (a folded line), by another '\r' (an empty line
// next), or by a byte that can begin a field name — in that last case the
// next candidate is probed, and the '\r' may still turn out to be content; see
// take. Any other '\r' is content of the line, as it always was. Each line
// ending is recorded in scan.ends once it is known to be one.
//
// It reads the raw bytes rather than through bufio.Reader.ReadLine because a
// budget has to be charged what was actually consumed, and ReadLine reports a
// line whose line ending it has already stripped.
//
// A line the input ended with io.EOF before any line ending is kept, and
// recorded as unterminated. A line a read failure other than io.EOF cut short
// is not a line: it is returned as line was given, without the bytes of the cut
// line, together with that failure, so that nothing the input did not end is
// read as though it had been, and the failure is not left for a later read
// that bufio would not repeat. A '\r' whose next byte that failure prevented
// reading is a byte of the cut line. Its bytes are still charged first, so a
// budget they exceed is what is reported.
func readLineSlice(r *bufio.Reader, line []byte, scan *headerScan) ([]byte, error) {
	given := len(line)

	scan.beginLine()
	// Where this line's exact bytes begin in the capture of a malformed prefix
	// that may still grow; see headerScan.capture.
	capStart := len(scan.capture)

	for {
		b, end, next, err := scan.nextChunk(r)

		// The budgets are charged before the bytes are kept, so a line longer
		// than a budget is refused after one buffer rather than accumulated:
		// what this appends never exceeds the budget by more than that buffer.
		// The exact bytes a prefix captures are kept on the same terms, after
		// the charge that admitted them, line endings included.
		switch end {
		case endNone:
			if terr := scan.take(b, 0, false); terr != nil {
				return line, terr
			}
			scan.keep(b)
			line = append(line, b...)

		case endLF, endCRLF:
			kind, term := LineEndingLF, 1
			if end == endCRLF {
				kind, term = LineEndingCRLF, 2
			}
			content := b[:len(b)-term]
			if terr := scan.take(content, int64(term), true); terr != nil {
				return line, terr
			}
			scan.keep(b)
			scan.lineKept(capStart)
			scan.ends.note(kind, scan.consumed-int64(term))
			if end == endCRLF {
				scan.noteTerm(termCRLF)
			} else {
				scan.noteTerm(termLF)
			}
			return append(line, content...), nil

		case endCR:
			content := b[:len(b)-1]
			atStart := len(content) == 0 && scan.consumed == scan.lineStart
			nb := byte(next)
			ending := atStart || next == eofNext || nb == '\r' || isSpace(nb) || validHeaderKeyByte(nb)
			if !ending {
				// Content, as a '\r' inside a line always was.
				if terr := scan.take(b, 0, false); terr != nil {
					return line, terr
				}
				scan.keep(b)
				line = append(line, b...)
				continue
			}
			if terr := scan.take(content, 1, true); terr != nil {
				return line, terr
			}
			scan.keep(b)
			scan.lineKept(capStart)
			// The '\r' is this line's last byte whether it proves to end the
			// line or to be content the next candidate joins to it.
			scan.noteTerm(termCR)
			cr := scan.consumed - 1
			if !atStart && next != eofNext && validHeaderKeyByte(nb) {
				// Whether this '\r' ended the line is settled by the next
				// candidate; it is recorded then, if it did.
				scan.crKey = crLine{
					ok:         true,
					lineStart:  scan.lineStart,
					lineBytes:  scan.lineBytes,
					fieldStart: scan.fieldStart,
					fieldBytes: scan.fieldBytes,
					cr:         cr,
				}
			} else {
				scan.ends.note(LineEndingCR, cr)
			}
			return append(line, content...), nil

		case endInput:
			if err == io.EOF {
				if len(b) == 0 && scan.consumed == scan.lineStart {
					// Nothing of a line: the input ended between lines.
					return line, err
				}
				// The input ended without a line ending. The last line is
				// reported with the bytes it did read, and the end is left for
				// the next read, which the scanner answers from its recorded
				// io.EOF.
				if terr := scan.take(b, 0, true); terr != nil {
					return line, terr
				}
				scan.keep(b)
				scan.lineKept(capStart)
				scan.noteTerm(termNone)
				scan.ends.unterminated = true
				scan.ends.untermStart, scan.ends.untermEnd = scan.lineStart, scan.consumed
				return append(line, b...), nil
			}
			// Cut by a read failure, possibly after earlier buffers of the
			// same long line: none of it is kept, and none of its bytes stay
			// in a prefix's capture either.
			if terr := scan.take(b, 0, false); terr != nil {
				return line, terr
			}
			scan.unkeep(capStart)
			if scan.probe != probeOff || scan.lineRejoined {
				// The '\r' before this line was content, or was never settled:
				// the line it ended is cut with this one.
				scan.dropPrev = true
			}
			scan.probe = probeOff
			return line[:given], err
		}
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

// hasContinuationLine reports whether the next byte of r begins a continuation
// line, without consuming it. A failed look is no continuation: the scanner has
// recorded the failure, and the next read of the block reports it.
func hasContinuationLine(r *bufio.Reader, scan *headerScan) bool {
	b, err := scan.vpeek(r, 1)
	if err != nil || len(b) == 0 {
		return false
	}
	return isSpace(b[0])
}

// vpeek is peek over the input as the scanner reads it: the replay, if any,
// then r. n is at most 2. Nothing is consumed.
func (s *headerScan) vpeek(r *bufio.Reader, n int) ([]byte, error) {
	if len(s.replay) == 0 {
		return s.peek(r, n)
	}
	if len(s.replay) >= n {
		return s.replay[:n], nil
	}
	b, err := s.peek(r, n-len(s.replay))
	out := append(append(s.vbuf[:0], s.replay...), b...)
	return out, err
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
// line on its own, and so does a '\r': with the '\n' after it as "\r\n", and
// otherwise — whatever byte follows it, or io.EOF — as a bare '\r', since no
// field can begin with one. Anything else is the first byte of a field's first
// line. A '\r' whose next byte could not be looked at is not known to end
// anything, so it begins a candidate, whose read then meets the failure.
//
// Nothing is read ahead here that the parser was not about to read anyway:
// these are the first bytes of the line it goes on to read either way, and Peek
// leaves every one of them for it. The same look-ahead already happens one line
// further on, in hasContinuationLine.
//
// candidate reports the other outcome of the same look: a byte is there and it
// does not begin that empty line, so a field candidate begins here. An input
// with no byte left — its end, or a failed read — is neither. A failed look is
// recorded by the scanner, and the read that follows reports it.
func atBlockEnd(r *bufio.Reader, scan *headerScan) (end, candidate bool) {
	// The replay, if any, is the next bytes of the input: a line taken out to
	// be decided as a delimiter begins with "--", and lines settleCR took out
	// for a record begin with a name byte or white space, so both begin a
	// candidate or a continuation, never the empty line; they are looked at
	// as any bytes are.
	b, err := scan.vpeek(r, 1)
	if err != nil || len(b) == 0 {
		return false, false
	}
	switch b[0] {
	case '\n':
		return true, false
	case '\r':
		b, err := scan.vpeek(r, 2)
		if len(b) == 2 || (err == io.EOF && len(b) == 1) {
			return true, false
		}
	}
	return false, true
}

// readContinuedLineSlice reads the next logical field of the block — its first
// physical line and every continuation line — or the empty line that ends the
// block, and returns it with every physical line ended by "\r\n", together with
// where its last physical line begins in what it returns.
//
// A candidate that follows a line ended by a '\r' that may be content is probed
// while its first line is read (see take); when that '\r' turns out to be
// content, scan.rejoined reports that what is returned continues the previous
// field rather than beginning one.
func readContinuedLineSlice(r *bufio.Reader, scan *headerScan) ([]byte, int, error) {
	// The line endings noted from here on are this candidate's.
	scan.resetTerms()

	// The empty line that ends the block is a line of the block but not a
	// logical field, so it is read outside any field's budget. Settling that
	// here — from bytes that are looked at and not consumed — is what keeps a
	// field budget from being spent on a field that is not there.
	end, candidate := atBlockEnd(r, scan)
	if end {
		scan.beginBlockEnd()
		line, err := readLineSlice(r, nil, scan)
		return line, 0, err
	}

	// A candidate that the shared field budget has no room for is refused here,
	// from the byte that proves it exists, rather than after it has been read
	// whole: no per-block budget may be in force to bound how long it is.
	//
	// A candidate that a '\r' may still prove to be content is refused here
	// like any other, so an exhausted shared field budget also refuses such a
	// '\r' and what follows it, even where they would have continued the
	// previous field: a budget is never spent later than it would have been.
	if candidate {
		if err := scan.preflightField(); err != nil {
			return nil, 0, err
		}
	}

	// A logical field begins here and ends where this call does, so its budget
	// covers this line and every continuation line read below.
	scan.beginField()

	// The line before ended with a '\r' that only this candidate can settle,
	// so its first line is probed while it is read.
	scan.probePrev, scan.crKey = scan.crKey, crLine{}
	if scan.probePrev.ok {
		scan.probe = probeName
	}

	// Read the first line. We preallocate slice that it enough
	// for most fields.
	line, err := readLineSlice(r, make([]byte, 0, 256), scan)
	scan.probe = probeOff
	if err == io.EOF && len(line) == 0 {
		// Header without a body
		return nil, 0, nil
	} else if err != nil {
		return nil, 0, err
	}

	if len(line) == 0 { // blank line - no continuation
		return line, 0, nil
	}

	line = append(line, '\r', '\n')
	last := 0

	// Read continuation lines.
	for hasContinuationLine(r, scan) {
		start := len(line)
		line, err = readLineSlice(r, line, scan)
		if err != nil {
			if isBudgetExhausted(err) {
				// The budget is a hard stop rather than a read that can be
				// retried: the bytes it refused are already gone, so the field
				// is not handed back half read. A read failure is not such a
				// stop, however it presents itself, and keeps the long-standing
				// behavior below.
				return nil, 0, err
			}
			// A read failure cut this continuation line. The field is kept
			// with the lines read whole before it — line holds exactly those —
			// and the scanner has recorded the failure, which the next read of
			// the block reports.
			break
		}

		line = append(line, '\r', '\n')
		last = start
	}

	return line, last, nil
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
//
// A read of r that fails is reported, never skipped. The first error any read
// of the block returns, io.EOF included, is remembered for the rest of the
// block: from then on nothing more is asked of r's source, and only the bytes r
// already holds are read. A failure other than io.EOF is returned as the very
// value r returned, with the fields read before it — every field all of whose
// lines were read whole, including one whose next line could not be looked at,
// and a field whose continuation line the failure cut, kept with its lines
// before that one. A line the failure cut is not kept, and no terminator is
// supplied for it. A block that ended before the failure was reached, with the
// failure still among bytes r holds past the empty line, is read normally, and
// the failure is what the next read of r returns. io.EOF keeps its
// long-standing meaning: a block that ends at the end of the input is a header
// with no body, and its last line may lack a terminator. Before this, a failed
// look-ahead was ignored and the source read again, so a one-shot failure could
// vanish, or a cut line be reported as a malformed one.
//
// An empty input and an input that is only the empty line are both an empty
// Header and a nil error, and complete fields that io.EOF ended are the same
// fields as with an empty line after them. What follows that empty line, blank
// lines included, is left in r. WriteHeader always ends what it writes with one
// empty line, so a block io.EOF ended is written with one (ROB-010-001).
//
// A source may fail with bufio.ErrBufferFull itself, the value ReadSlice also
// uses for a line longer than r's buffer. The scanner never asks r for more
// than its buffer holds, so that value is always the source's failure and is
// reported as above, never read on.
//
// # Line endings
//
// A physical line ends at "\r\n" — one line ending even when its two bytes
// arrive in different reads — or at a bare '\n', or at a bare '\r' (one not
// followed by '\n'). Which one ended each line, and where, is reported by
// Header.LineEndings; the raw bytes of every field end each of its physical
// lines with "\r\n" whatever ended it in the input. A complete field ended by
// "\r\n" keeps its bytes exactly; one ended otherwise is normalized and is not
// a byte-exact copy of its input.
//
// A bare '\r' ends a line when it begins the line (the empty line that ends
// the block), when io.EOF follows it (so "A: 1\r" at the end of the input is a
// complete field), and when it is followed by white space (the next line
// continues the field) or by another '\r' (so "A: 1\r\r\n" is field A ended by
// a '\r', then the empty line "\r\n"). Any other bare '\r' is content of the
// line, as it always was — with one case decided by what follows: a '\r'
// followed by a byte that can begin a field name ends the line only if the
// next line begins a field, 1*name-byte *WSP ":". Otherwise that '\r' and the
// next line are content of the line it was in, so "Subject: a\rb" stays one
// field whose value is "a\rb", exactly as before. That is how a header whose
// lines all end with '\r' is read as fields, while a stray '\r' in a value
// keeps its long-standing meaning. A stray '\r' before text that looks like a
// field, "Subject: a\rTo: b", is a line ending: no rule can tell such a line
// from a field of a header whose lines end with '\r'.
//
// One byte after a '\r' is looked at before it is consumed, and only that
// byte: so a header whose last line ending is a bare '\r' is still being read
// until the byte after it, or io.EOF, arrives, and a failure there is reported
// as a failure of the block. A '\r' that a failure other than io.EOF follows is
// a byte of the line the failure cut, and so is a '\r' the next line was still
// deciding when the failure came.
//
// Budgets count the input's own bytes. A line ending is not content of its
// line; it is counted as bytes of its field and of the block. A '\r' that is
// content is content of its line, and a line it joins to the next is one
// physical line, counted whole: when the next line turns out to continue it,
// the line's and the field's running totals become the aggregate of both — the
// line's bytes, the '\r' as content, and the next line's — which is rechecked
// at once, refusing where the line or the field began if it exceeds
// MaxLineBytes or MaxFieldBytes, and goes on being charged as one. No byte is
// charged twice: the block and shared budgets saw each byte once, when it was
// read, and the '\r' only moves from line ending to content. A next line that is still
// deciding is charged as a line of its own until it decides, so it never
// reads more than one such line past a budget. Field counts are never
// refunded: an exhausted shared field budget refuses the next line before any
// of it is read, even one that would have continued the previous field.
//
// bufio.Reader hands out a read error once. After ReadHeader has returned one,
// r no longer holds it, and reading r again reads its source again; a caller
// that must not do so stops reading r, or gives it a source that keeps failing.
//
// # Byte order marks
//
// A block may begin with a byte order mark (ROB-010-003); see bom.go and
// Header.ByteOrderMark. A UTF-8 mark is consumed, counted as bytes of the block
// and kept by the Header, and the fields are read after it. A UTF-16 or UTF-32
// mark leaves the block unread: ReadHeader consumes nothing and returns a
// Header with no fields that reports the mark, and a nil error, so that
// everything from the mark on is what r yields next. The text after such a
// mark is never transcoded. Only the first bytes of a block are looked at, at
// most four, before anything else about the block is decided; see bom.go for
// what that look-ahead reads and which refusal it can precede.
//
// # Malformed prefixes
//
// A block whose first line — after any UTF-8 mark — begins with white space
// begins with no field. Its first logical candidate, that line and the
// continuation lines after it, is kept apart as the Header's malformed prefix,
// byte for byte, and is no field; the block is read on after it (ROB-020-001).
// See prefix.go and Header.MalformedPrefix. Before, such a block was refused
// with a "malformed MIME header initial line" error and no fields; that error
// is no longer returned.
//
// # Orphan continuation lines
//
// A damaged line — no colon or semicolon, an invalid name, or an empty one —
// followed by at least one continuation line read whole is kept, with those
// lines, apart from the fields as an orphan record, byte for byte, and the
// block is read on after it (ROB-020-002). See orphan.go and
// Header.OrphanContinuations. Before, such a block was refused with a
// "malformed MIME header line" or "malformed MIME header key" error, or, for
// an empty name, lost the lines silently. A damaged line with no continuation
// line is read as it always was.
func ReadHeader(r *bufio.Reader) (Header, error) {
	return readHeader(r, &headerScan{})
}

// ReadHeaderLimit reads a MIME header from r like ReadHeader, but refuses one
// whose wire bytes exceed maxBytes.
//
// What is counted is the header block exactly as it appears in the input, from
// the first byte of its first field — or of the UTF-8 byte order mark before it
// — through the empty line that ends it, that empty line included. A block of
// exactly maxBytes bytes is read normally; one
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
// budget is retained — with one qualification for a line after a bare '\r'
// that is still deciding whether it continues the field before it (see
// ReadHeader): while it decides, the previous line and field and that one
// candidate line are each held to MaxLineBytes and MaxFieldBytes on their own,
// and their joined total is checked once the candidate proves to continue the
// field, so up to one such candidate (itself bounded by those budgets and one
// buffer) may be read past the joined line's or field's budget before that
// refusal; MaxBytes and the shared budget count every byte as it is read and
// are never exceeded by more than one buffer; and a block that overran leaves r where the failure
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
// already rely on. For a read failure, what it read is the fields whose lines
// were read whole; see ReadHeader.
func headerResult(fs []*headerField, err error) (Header, error) {
	if isBudgetExhausted(err) {
		return newHeader(nil), err
	}
	return newHeader(fs), err
}

// readHeader reads one header block from r: the byte order mark it may begin
// with (see bom.go), and then its fields.
//
// A block that begins with a UTF-16 or UTF-32 mark is not read at all: nothing
// is consumed or charged, and the Header that reports the mark has no fields.
// A UTF-8 mark is consumed and charged as bytes of the block, and the fields
// are read after it. The Header keeps the mark whenever it keeps its fields —
// with an ordinary failure too, beside the fields read whole before it — and
// is the empty Header, without the mark, when a budget refused the block.
func readHeader(r *bufio.Reader, scan *headerScan) (Header, error) {
	bom := scan.byteOrderMark(r)
	switch {
	case bom.wide():
		return Header{bom: bom}, nil
	case bom == BOMUTF8:
		n := len(bomBytes[BOMUTF8])
		r.Discard(n)
		// Bytes of the block, but of no line and no field: the line and the
		// field that follow begin after it, and neither has begun yet.
		if err := scan.charge(0, int64(n)); err != nil {
			return headerResult(nil, err)
		}
	}
	h, err := readFields(r, scan)
	if !isBudgetExhausted(err) {
		h.bom = bom
	}
	return h, err
}

// readFields reads the fields of a header block, and the empty line that ends
// it, from where r stands.
func readFields(r *bufio.Reader, scan *headerScan) (Header, error) {
	fs := make([]*headerField, 0, 32)

	// A field cannot begin with white space. A block whose first line does
	// anyway begins with a malformed prefix: its first logical candidate — that
	// line and every continuation line after it, and whatever a '\r' joins to
	// them — is kept apart, byte for byte, rather than read as a field, and the
	// block goes on after it (ROB-020-001; see prefix.go). A failed look is
	// recorded by the scanner and reported by the read below.
	prefixed := false
	prefixStart := scan.consumed
	if buf, err := scan.peek(r, 1); err == nil && isSpace(buf[0]) {
		prefixed = true
		scan.capturing = true
	}
	first := true

	// The last field read is held as pending rather than built at once, because
	// the next candidate may still continue it (a '\r' that turns out to be
	// content; see take). It is built exactly once, when what follows settles
	// that it is complete, so a run of such continuations costs one pass over
	// its bytes. A malformed prefix is held the same way, and is built into
	// prefix instead of a field; so is an orphan record, built into orphans
	// (ROB-020-002; see orphan.go), and a damaged line still waiting for a
	// '\r' to say whether a continuation follows it, which is kept only if
	// one does.
	var pend pendingField
	var prefix *malformedPrefix
	var orphans []*orphanRecord
	// recovered reports that an orphan record has been recovered in this
	// block: in a part's header every later line is then first looked at as
	// a delimiter line, as after a malformed prefix.
	recovered := false
	finish := func() {
		switch {
		case pend.kv == nil:
		case pend.kind == pendPrefix:
			prefix = scan.prefixOf(prefixStart, pend.kv)
		case pend.kind == pendRecord:
			if o := pend.record(len(fs)); o != nil {
				orphans = append(orphans, o)
			}
		case pend.kind == pendDamaged:
			// A damaged line that nothing continued: no field, and not kept,
			// as before. Every path that must still report it as an error
			// settles it before it gets here.
		default:
			fs = append(fs, pend.field(len(fs)+1))
		}
		pend = pendingField{}
	}
	// result is headerResult with the prefix and the orphan records: a block
	// a budget refused keeps none of them.
	result := func(err error) (Header, error) {
		h, err := headerResult(fs, err)
		if !isBudgetExhausted(err) {
			h.prefix = prefix
			h.orphans = orphans[:len(orphans):len(orphans)]
			if scan.heal != (headerHeal{}) {
				heal := scan.heal
				h.heal = &heal
			}
		}
		return h, err
	}
	// settleDamaged ends a damaged line held for its '\r' now that no
	// continuation line follows it: it is decided as it always was, with no
	// continuation — an empty name is skipped, which is nil, and any other is
	// the error it always was, built when the line was read.
	settleDamaged := func() error {
		err := pend.headErr
		pend = pendingField{}
		return err
	}
	// unseen returns the read failure, other than io.EOF, that a read of the
	// block has observed, or nil. A damaged line read whole whose continuation
	// lines could not be looked at — the look past it, or the line after it,
	// met that failure — is not known to have none: the failure is what is
	// returned, as itself, rather than the error the line would be if nothing
	// continued it.
	unseen := func() error {
		if scan.fail != nil && scan.fail != io.EOF {
			return scan.fail
		}
		return nil
	}
	// ended returns the header of a block that ended without a failure of its
	// own: at its empty line, at the end of the input, or at a delimiter.
	ended := func() (Header, error) {
		var err error
		if scan.fail != nil && scan.fail != io.EOF {
			// A read of this block failed, and nothing since has reported
			// it. Only a failure a read of the block itself observed is
			// meant: one still buffered past the empty line that ends the
			// block was never observed and belongs to what follows.
			err = scan.fail
		}
		finish()
		h, err := result(err)
		if err == nil {
			ends := scan.ends
			ends.known = true
			h.ends = &ends
		}
		return h, err
	}

	for {
		// In a part's header that began with a malformed prefix, or in which
		// an orphan record has been recovered, the next line is looked at as a
		// delimiter of the multipart before it is read as anything else. A
		// proved one is never read as a field or as part of one: the block ends
		// before it, and the part's body is empty. A '\r' that ended the line
		// before it, and was still waiting for that line to say whether it was
		// content, was a line ending. A line that is not proved one is read
		// below, from its first byte, as it always was. See prefix.go and
		// orphan.go.
		//
		// The lines a '\r' that is content joins to a damaged line that
		// settleCR proved a record (a pendDamaged with an error, which is held
		// only once proved) are not looked at here: settleCR already looked at
		// every line of them as a delimiter line, and they may be in the
		// replay, ahead of what r holds. Nothing is looked at while a replay
		// is still to be read, for the same reason.
		provedJoin := pend.kind == pendDamaged && pend.headErr != nil
		if scan.parent != nil && len(scan.replay) == 0 && !provedJoin && ((prefixed && !first) || recovered) {
			look, m := scan.parentDelimiter(r)
			proved := look == lookProved
			if look == lookLong {
				var herr error
				if proved, herr = scan.holdDelimiter(r, m); herr != nil {
					finish()
					return result(herr)
				}
			}
			if proved {
				if c := scan.crKey; c.ok {
					scan.ends.note(LineEndingCR, c.cr)
					scan.crKey = crLine{}
				}
				scan.atDelim = true
				scan.endCapture(len(scan.capture))
				return ended()
			}
		}

		mark := len(scan.capture)
		endsBefore := scan.ends
		kv, last, err := readContinuedLineSlice(r, scan)
		rejoined, dropPrev := scan.rejoined, scan.dropPrev
		scan.rejoined, scan.dropPrev = false, false
		if dropPrev {
			// A failure cut the line a '\r' that was content, or was never
			// settled, ended: that line is cut with the line after it. Its
			// bytes leave a prefix's capture, or an orphan record's exact
			// bytes, with it.
			if pend.kind == pendPrefix {
				scan.unkeep(scan.capLast)
			}
			pend.dropLastLine()
		}

		if first {
			first = false
			if prefixed && len(kv) > 0 {
				// The malformed prefix, read whole as one logical candidate:
				// charged to the field count once, like any candidate, and
				// held until what follows settles that nothing continues it.
				if cerr := scan.chargeField(); cerr != nil {
					return result(cerr)
				}
				pend = pendingField{kv: kv, lastLine: last, kind: pendPrefix}
				continue
			}
		} else if scan.capturing && !(rejoined && len(kv) > 0 && pend.kind == pendPrefix) {
			// What this read is not the prefix's: its bytes leave the capture,
			// and the prefix can no longer grow.
			scan.endCapture(mark)
		}

		if len(kv) == 0 {
			scan.endCapture(len(scan.capture))
			if err == nil {
				if pend.kind == pendDamaged {
					if derr := settleDamaged(); derr != nil {
						return result(derr)
					}
				}
				return ended()
			}
			finish()
			return result(err)
		}

		if rejoined {
			// Not a field: the rest of the field before it, read whole. It
			// was never charged to the field count, and is not now. An orphan
			// record, or a damaged line held for its '\r', takes the exact
			// bytes too.
			var raw []byte
			rawLast := 0
			if pend.rawKind() {
				raw, rawLast = scan.exact(kv)
			}
			pend.join(kv, last, raw, rawLast)
			if pend.kind == pendDamaged {
				switch {
				case pend.lastLine > 0:
					// The line the '\r' joined to it brought a continuation
					// line with it, read whole: an orphan record after all.
					pend.kind = pendRecord
					recovered = true
				case !scan.crKey.ok:
					// Still no continuation, and no '\r' left to bring one.
					if pend.headErr != nil {
						if ferr := unseen(); ferr != nil {
							pend = pendingField{}
							return result(ferr)
						}
					}
					if derr := settleDamaged(); derr != nil {
						return result(derr)
					}
				}
			}
			continue
		}

		// A damaged line held for its '\r' is not continued by this candidate:
		// the '\r' ended it, and nothing continues it.
		if pend.kind == pendDamaged {
			if derr := settleDamaged(); derr != nil {
				return result(derr)
			}
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
			return result(cerr)
		}

		head, key, i := classifyCandidate(kv)
		if head == headField {
			// The field before this one is complete now.
			finish()
			pend = pendingField{key: key, kv: kv, valueStart: i + 1, lastLine: last}
			continue
		}

		// Under recovery, a damaged line that reads as a continuation line of
		// the field before it that lost its leading white space is that
		// field's next line, exactly as read (see recover.go).
		healing := scan.lim.Recover && head != headEmptyName && !scan.crKey.ok && len(scan.replay) == 0
		if healing && pend.unindented(kv) {
			if scan.heal.joined == 0 {
				scan.heal.joinedAt = scan.fieldStart
			}
			scan.heal.joined++
			base := len(pend.kv)
			pend.kv = append(pend.kv, kv...)
			pend.lastLine = base + last
			continue
		}
		// Whether the line before it held only white space, for a block that
		// recovery ends here.
		wsBefore := healing && pend.kind == pendField && pend.lastLine > 0 && blankLine(pend.kv[pend.lastLine:])

		// A damaged line: no colon or semicolon, an invalid name, or an empty
		// one, which nothing here makes a field. The field before it is
		// complete either way.
		finish()
		start := scan.fieldStart
		switch {
		case last > 0:
			// Continuation lines followed it, read whole: it and they are an
			// orphan record, kept apart byte for byte, and the block goes on
			// (ROB-020-002; see orphan.go). It is held, as a field is, for a
			// '\r' that may still join the next line to it.
			raw, rawLast := scan.exact(kv)
			pend = pendingField{kv: kv, lastLine: last, kind: pendRecord,
				raw: raw, rawLast: rawLast, start: start}
			recovered = true
		case scan.crKey.ok:
			// Its line ended with a '\r' that the next line settles: that
			// line may still join it, and bring continuation lines. An empty
			// name, which was always read on past (its joined lines skipped
			// with it, the next field read as a field), is held as before.
			// Any other damaged line was refused here, with the next line
			// left unread: it is read on only once settleCR has proved,
			// within what the budgets admit, that the bytes after the '\r'
			// join it and bring a continuation line. Otherwise it is the
			// error it always was, everything past its '\r' still in r —
			// unless the shared count refuses first, or the look met a read
			// failure, which is returned as itself.
			derr := damageError(head, kv, i)
			if derr != nil {
				cont, ferr := scan.settleCR(r)
				if ferr != nil {
					return result(ferr)
				}
				if !cont {
					return result(derr)
				}
			}
			raw, rawLast := scan.exact(kv)
			pend = pendingField{kv: kv, lastLine: last, kind: pendDamaged,
				raw: raw, rawLast: rawLast, start: start, headErr: derr}
		case head == headEmptyName:
			// As per RFC 7230 field-name is a token, tokens consist of one or
			// more chars. An empty name with nothing continuing it is skipped,
			// as it always was, under recovery too: the budget tests count on
			// such a line costing a candidate and nothing else.
		default:
			// Nothing continues it: the error it always was — unless a read
			// failure kept its continuation lines from being looked at.
			if ferr := unseen(); ferr != nil {
				return result(ferr)
			}
			if !healing {
				return result(damageError(head, kv, i))
			}
			// Recovery: the block goes on after it, or ends before it
			// (ROB-020-003 to ROB-020-006; see recover.go).
			on, ferr := scan.goesOn(r)
			if ferr != nil {
				return result(ferr)
			}
			raw, rawLast := scan.exact(kv)
			if on {
				scan.heal.damaged++
				pend = pendingField{kv: kv, lastLine: last, kind: pendRecord,
					raw: raw, rawLast: rawLast, start: start, single: true}
				recovered = true
				continue
			}
			// The first line of the body: it is handed back, and the block
			// ends before it, its line ending never one of the block's.
			scan.unread = raw
			scan.consumed -= int64(len(raw))
			scan.ends = endsBefore
			scan.heal.sepMissing, scan.heal.sepAt, scan.heal.wsLine = true, start, wsBefore
			return ended()
		}
	}
}

// A candidateHead is what a logical candidate's first bytes make it, by the
// rules a header block has always been read with: the name is everything
// before the first colon of the whole candidate — or, with no colon in it, the
// first semicolon — less white space around it, and it must be one or more
// printable US-ASCII bytes other than the colon (RFC 5322 section 2.2).
type candidateHead uint8

const (
	// headField: a field, read as one.
	headField candidateHead = iota
	// headNoSeparator: neither a colon nor a semicolon anywhere in it.
	headNoSeparator
	// headInvalidName: a name with a byte a name cannot hold — white space,
	// a control byte, an 8-bit byte, or a line ending when the separator was
	// found only on a continuation line.
	headInvalidName
	// headEmptyName: nothing before the separator but white space.
	headEmptyName
)

// classifyCandidate reads kv, a logical candidate read whole, on those rules:
// what it is, and for a field its canonical key; i is where its separator is,
// or -1 when it has none.
func classifyCandidate(kv []byte) (head candidateHead, key string, i int) {
	// Key ends at first colon; should not have trailing spaces but they
	// appear in the wild, violating specs, so we remove them if present.
	i = bytes.IndexByte(kv, ':')
	if i < 0 {
		// Some malformed messages use a semicolon instead of a colon as
		// the field separator; accept it as a fallback.
		i = bytes.IndexByte(kv, ';')
		if i < 0 {
			return headNoSeparator, "", -1
		}
	}

	keyBytes := trim(kv[:i])

	// Verify that there are no invalid characters in the header key.
	// See RFC 5322 Section 2.2
	for _, c := range keyBytes {
		if !validHeaderKeyByte(c) {
			return headInvalidName, "", i
		}
	}
	if len(keyBytes) == 0 {
		return headEmptyName, "", i
	}
	return headField, textproto.CanonicalMIMEHeaderKey(string(keyBytes)), i
}

// damageError returns the error a damaged candidate kv that nothing
// continues has always been refused with, i being where classifyCandidate
// found its separator; nil for an empty name, which has always been skipped.
func damageError(head candidateHead, kv []byte, i int) error {
	switch head {
	case headNoSeparator:
		return fmt.Errorf("message: malformed MIME header line: %v", string(kv))
	case headInvalidName:
		return fmt.Errorf("message: malformed MIME header key: %v", string(trim(kv[:i])))
	}
	return nil
}

// A pendKind is what a pendingField will be built into.
type pendKind uint8

const (
	// pendField: a field.
	pendField pendKind = iota
	// pendPrefix: the block's malformed prefix, built from the exact bytes
	// the scanner captured. See prefix.go.
	pendPrefix
	// pendRecord: an orphan record — a damaged line and the continuation
	// lines after it. See orphan.go.
	pendRecord
	// pendDamaged: a damaged line with no continuation line yet, whose last
	// line ended with a '\r' the next line settles. It becomes a pendRecord
	// if that line joins it and brings a continuation line, and is otherwise
	// decided as a damaged line always was; see headErr.
	pendDamaged
)

// A pendingField is a field read whole but not yet built, because the next
// candidate may still continue it.
type pendingField struct {
	key string

	// kv is the raw field so far, every physical line ended by "\r\n";
	// valueStart is where its value begins, after the colon or semicolon, and
	// lastLine where its last physical line begins.
	kv         []byte
	valueStart int
	lastLine   int

	// kind is what it will be built into. A prefix, a record and a damaged
	// line have no key and no value.
	kind pendKind

	// For a record and a damaged line: raw is its exact bytes so far, every
	// line ending as it was read, and rawLast where its last physical line
	// begins in raw; start is where it began, relative to the block.
	raw     []byte
	rawLast int
	start   int64

	// headErr is, for a damaged line, the error it is decided with if
	// nothing continues it, or nil for an empty name, which is skipped.
	headErr error

	// single marks a record that recovery keeps though no continuation line
	// followed its damaged line. See recover.go.
	single bool
}

// rawKind reports whether p keeps exact bytes of its own: an orphan record,
// or a damaged line that may become one.
func (p *pendingField) rawKind() bool {
	return p.kind == pendRecord || p.kind == pendDamaged
}

// join appends cand, a candidate that a '\r' made the continuation of this
// field's last physical line, to the field: the "\r\n" that line was given is
// replaced by the '\r' that was content, followed by cand as it was read. cand
// is copied once; nothing the field already holds is copied again but by the
// growth of its buffer. For a record or a damaged line, raw is cand's exact
// bytes and rawLast where its last physical line begins in them: the '\r' is
// already the last byte of p.raw, as read, so they are appended as they are.
func (p *pendingField) join(cand []byte, candLast int, raw []byte, rawLast int) {
	if p.kv == nil {
		return
	}
	base := len(p.kv) - len("\r\n")
	p.kv = append(p.kv[:base], '\r')
	p.kv = append(p.kv, cand...)
	if candLast > 0 {
		p.lastLine = base + 1 + candLast
	}
	if p.rawKind() {
		if candLast > 0 {
			p.rawLast = len(p.raw) + rawLast
		}
		p.raw = append(p.raw, raw...)
	}
}

// dropLastLine removes the field's last physical line, which a failure cut;
// a field left with no line is no field. A record or a damaged line loses the
// line's exact bytes with it.
func (p *pendingField) dropLastLine() {
	if p.lastLine == 0 {
		*p = pendingField{}
		return
	}
	p.kv = p.kv[:p.lastLine]
	if p.rawKind() {
		p.raw = p.raw[:p.rawLast]
	}
}

// field builds the field, once; seq is its place among the fields of its
// block, counting from 1.
func (p *pendingField) field(seq int) *headerField {
	f := newHeaderField(p.key, trimAroundNewlines(p.kv[p.valueStart:]), p.kv)
	f.seq = seq
	return f
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
//
// A header whose block began with a UTF-8 byte order mark is written with the
// mark first, once, and then its fields; see Header.ByteOrderMark. A header
// whose block began with a UTF-16 or UTF-32 mark is refused with an error
// before anything is written: that block was never read, and neither its
// fields nor its mark are this header's to write.
//
// A header whose block began with a malformed prefix writes it right after any
// UTF-8 mark and before every field, so that it can never become the
// continuation of a field — not even of one added since, which Add puts at the
// top of the fields, after the prefix. Every byte of the prefix is written, in
// order, its line endings as they were read; only a last line the prefix did
// not end with "\r\n" or "\n" is given one: a last line the input ended with
// no line ending is followed by "\r\n", and one ended by a bare '\r' by "\n",
// so that what follows can never be read as a continuation of that line. See
// MalformedPrefix.
//
// A header whose block held orphan records writes each in its place among the
// fields read with it: right before the first of the fields that followed it
// in the block that the header still has, or after every field when none is
// left. Fields added since (Add, AddRaw, Set) are at the top, as always, and
// so are written before every record; deleting a field never moves a record
// among the fields read with it, nor drops one. Each record is written as its
// prefix is, every byte in order and only a last line without a "\r\n" or
// "\n" given one. A record begins with its damaged line, never with white
// space, so it cannot become a continuation of the field written before it.
// See OrphanContinuation.
func WriteHeader(w io.Writer, h Header) error {
	if h.bom.wide() {
		return errWideHeader(h.bom)
	}
	if h.bom == BOMUTF8 {
		if _, err := io.WriteString(w, bomBytes[BOMUTF8]); err != nil {
			return err
		}
	}
	if err := h.prefix.writeTo(w); err != nil {
		return err
	}
	// next is the first record not yet written. Records are in the order
	// they were read, so their after counts never decrease.
	next := 0
	for i := len(h.l) - 1; i >= 0; i-- {
		f := h.l[i]
		if f.seq > 0 {
			for next < len(h.orphans) && h.orphans[next].after < f.seq {
				if err := writeRawLines(w, h.orphans[next].b); err != nil {
					return err
				}
				next++
			}
		}
		if rawField, err := f.raw(); err == nil {
			if _, err := w.Write(rawField); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("failed to write header field #%v (%q): %w", len(h.l)-i, f.k, err)
		}
	}
	for ; next < len(h.orphans); next++ {
		if err := writeRawLines(w, h.orphans[next].b); err != nil {
			return err
		}
	}

	_, err := w.Write([]byte{'\r', '\n'})
	return err
}
