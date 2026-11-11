// Package tnef reads Transport Neutral Encapsulation Format streams: the
// winmail.dat (application/ms-tnef) part Outlook and Exchange send in place of
// a message's formatted body and attachments. It follows MS-OXTNEF for the
// stream, the MAPI property encoding it carries, and MS-OXRTFCP for
// compressed RTF.
//
// The package is optional and stands alone: nothing else in the module imports
// it, and it imports only the standard library and golang.org/x/text. A caller
// that meets such a part hands its decoded bytes to Decode.
//
// Decode is tolerant. A stream that is cut short or damaged yields everything
// read up to the damage, with an Issue for each thing that was repaired,
// skipped or left out. The only error is ErrNotTNEF. Every length the input
// declares is checked against the input that is left and against Limits before
// anything is allocated for it.
package tnef

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// ErrNotTNEF is returned when the input does not begin with the TNEF
// signature. It is the only error Decode returns.
var ErrNotTNEF = errors.New("tnef: not a TNEF stream")

// Signature is the 32-bit value, little-endian, every TNEF stream begins with.
const Signature uint32 = 0x223E9F78

// The level byte of an attribute.
const (
	LevelMessage    byte = 0x01
	LevelAttachment byte = 0x02
)

// Attribute identifiers: the type in the high word, the id in the low word.
const (
	AttOwner                   uint32 = 0x00060000
	AttSentFor                 uint32 = 0x00060001
	AttDelegate                uint32 = 0x00060002
	AttDateStart               uint32 = 0x00030006
	AttDateEnd                 uint32 = 0x00030007
	AttAidOwner                uint32 = 0x00050008
	AttRequestRes              uint32 = 0x00040009
	AttOriginalMessageClass    uint32 = 0x00070600
	AttFrom                    uint32 = 0x00008000
	AttSubject                 uint32 = 0x00018004
	AttDateSent                uint32 = 0x00038005
	AttDateRecd                uint32 = 0x00038006
	AttMessageStatus           uint32 = 0x00068007
	AttMessageClass            uint32 = 0x00078008
	AttMessageID               uint32 = 0x00018009
	AttParentID                uint32 = 0x0001800A
	AttConversationID          uint32 = 0x0001800B
	AttBody                    uint32 = 0x0002800C
	AttPriority                uint32 = 0x0004800D
	AttAttachData              uint32 = 0x0006800F
	AttAttachTitle             uint32 = 0x00018010
	AttAttachMetaFile          uint32 = 0x00068011
	AttAttachCreateDate        uint32 = 0x00038012
	AttAttachModifyDate        uint32 = 0x00038013
	AttDateModified            uint32 = 0x00038020
	AttAttachTransportFilename uint32 = 0x00069001
	AttAttachRendData          uint32 = 0x00069002
	AttMAPIProps               uint32 = 0x00069003
	AttRecipTable              uint32 = 0x00069004
	AttAttachment              uint32 = 0x00069005
	AttTnefVersion             uint32 = 0x00089006
	AttOemCodepage             uint32 = 0x00069007
)

func knownAttr(id uint32) bool {
	switch id {
	case AttOwner, AttSentFor, AttDelegate, AttDateStart, AttDateEnd,
		AttAidOwner, AttRequestRes, AttOriginalMessageClass, AttFrom,
		AttSubject, AttDateSent, AttDateRecd, AttMessageStatus,
		AttMessageClass, AttMessageID, AttParentID, AttConversationID,
		AttBody, AttPriority, AttDateModified, AttMAPIProps, AttRecipTable,
		AttTnefVersion, AttOemCodepage:
		return true
	}
	return attachmentAttr(id)
}

func attachmentAttr(id uint32) bool {
	switch id {
	case AttAttachData, AttAttachTitle, AttAttachMetaFile,
		AttAttachCreateDate, AttAttachModifyDate,
		AttAttachTransportFilename, AttAttachRendData, AttAttachment:
		return true
	}
	return false
}

// Limits bounds what one Decode may read, keep and produce. A zero field takes
// its default; a negative field allows nothing of that kind. Byte limits above
// 2 GiB are taken as 2 GiB.
type Limits struct {
	// MaxInputBytes is how much of the input is read. The rest is ignored and
	// reported. Default 64 MiB.
	MaxInputBytes int64
	// MaxAttributes is how many attributes are read, over the stream and the
	// messages embedded in it. Default 16384.
	MaxAttributes int
	// MaxAttachments is how many attachments are kept, over the stream and the
	// messages embedded in it. Default 1024.
	MaxAttachments int
	// MaxProperties is how many properties one property list may hold.
	// Default 4096.
	MaxProperties int
	// MaxTotalProperties is how many properties, values of multi-valued
	// properties and recipient rows are kept in all. Default 65536.
	MaxTotalProperties int
	// MaxAttachmentBytes is how many bytes of one attachment are kept in
	// Attachment.Data. Default 32 MiB.
	MaxAttachmentBytes int64
	// MaxDecodedBytes is how many bytes are handed out in all: attachment
	// data, decoded text and decompressed RTF. Default 128 MiB.
	MaxDecodedBytes int64
	// MaxRTFBytes is how many bytes one compressed RTF body may decompress
	// to. Default 16 MiB.
	MaxRTFBytes int64
	// MaxBodyBytes is how many bytes of one text or HTML body are decoded,
	// counted as the stream holds them: UTF-8 can be up to three times as
	// long. Default 16 MiB.
	MaxBodyBytes int64
	// MaxStringBytes is how many bytes of one short string — a subject, a
	// name, a media type — are decoded, counted the same way. Default
	// 64 KiB.
	MaxStringBytes int64
	// MaxDepth is how deep embedded messages are read; the stream itself is
	// depth zero. Default 4, at most 32.
	MaxDepth int
	// MaxIssues is how many issues are kept. Default 256.
	MaxIssues int
}

func byteLimit(v, def int64) int64 {
	if v == 0 {
		v = def
	}
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return v
}

func countLimit(v, def, hard int) int {
	if v == 0 {
		v = def
	}
	if v < 0 {
		return 0
	}
	if v > hard {
		return hard
	}
	return v
}

func (l *Limits) resolved() Limits {
	var r Limits
	if l != nil {
		r = *l
	}
	r.MaxInputBytes = byteLimit(r.MaxInputBytes, 64<<20)
	r.MaxAttributes = countLimit(r.MaxAttributes, 16384, 1<<24)
	r.MaxAttachments = countLimit(r.MaxAttachments, 1024, 1<<20)
	r.MaxProperties = countLimit(r.MaxProperties, 4096, 1<<24)
	r.MaxTotalProperties = countLimit(r.MaxTotalProperties, 65536, 1<<24)
	r.MaxAttachmentBytes = byteLimit(r.MaxAttachmentBytes, 32<<20)
	r.MaxDecodedBytes = byteLimit(r.MaxDecodedBytes, 128<<20)
	r.MaxRTFBytes = byteLimit(r.MaxRTFBytes, 16<<20)
	r.MaxBodyBytes = byteLimit(r.MaxBodyBytes, 16<<20)
	r.MaxStringBytes = byteLimit(r.MaxStringBytes, 64<<10)
	r.MaxDepth = countLimit(r.MaxDepth, 4, 32)
	r.MaxIssues = countLimit(r.MaxIssues, 256, 1<<16)
	return r
}

// A Code names one kind of Issue. The values are stable.
type Code string

const (
	// CodeTruncated: the stream ends inside an attribute.
	CodeTruncated Code = "stream.truncated"
	// CodeDamaged: no attribute could be found after damaged bytes; reading
	// of that stream stopped there.
	CodeDamaged Code = "stream.damaged"
	// CodeResync: damaged bytes were skipped up to the next attribute that
	// verifies.
	CodeResync Code = "stream.resync"
	// CodeChecksum: an attribute's checksum does not match its data. The data
	// is used all the same.
	CodeChecksum Code = "attribute.checksum"
	// CodeLevel: an attribute's level byte does not fit its identifier.
	CodeLevel Code = "attribute.level"
	// CodeNoRendData: attachment attributes came without the attAttachRendData
	// that begins an attachment; one was begun for them.
	CodeNoRendData Code = "attachment.no_renddata"
	// CodePropertyTruncated: a property list ends inside a property.
	CodePropertyTruncated Code = "property.truncated"
	// CodePropertyType: a property has a type that cannot be sized; the rest
	// of its list is not read.
	CodePropertyType Code = "property.type"
	// CodeCodepageUnknown: text is in a code page this package cannot decode.
	CodeCodepageUnknown Code = "codepage.unknown"
	// CodeCodepageUndeclared: non-ASCII text came with no code page declared.
	CodeCodepageUndeclared Code = "codepage.undeclared"
	// CodeRTFDamaged: compressed RTF is cut short or malformed.
	CodeRTFDamaged Code = "rtf.damaged"
	// CodeRTFCRC: compressed RTF fails its CRC. It is decompressed all the
	// same.
	CodeRTFCRC Code = "rtf.crc"
	// CodeRTFCompression: compressed RTF declares an unknown compression type.
	CodeRTFCompression Code = "rtf.compression"
	// CodeRTFExtract: the text or HTML an RTF body carries was extracted only
	// in part.
	CodeRTFExtract Code = "rtf.extract"
	// CodeEmbeddedNotTNEF: an attachment declares an embedded message and
	// holds no TNEF stream.
	CodeEmbeddedNotTNEF Code = "embedded.not_tnef"

	CodeLimitInput           Code = "limit.input"
	CodeLimitAttributes      Code = "limit.attributes"
	CodeLimitAttachments     Code = "limit.attachments"
	CodeLimitProperties      Code = "limit.properties"
	CodeLimitAttachmentBytes Code = "limit.attachment_bytes"
	CodeLimitDecodedBytes    Code = "limit.decoded_bytes"
	CodeLimitRTFBytes        Code = "limit.rtf_bytes"
	CodeLimitBodyBytes       Code = "limit.body_bytes"
	CodeLimitStringBytes     Code = "limit.string_bytes"
	CodeLimitDepth           Code = "limit.depth"
)

// An Issue is one thing Decode repaired, skipped or left out.
type Issue struct {
	Code Code
	// Offset is where in the input the issue was found.
	Offset int64
	// Depth is zero for the stream itself and one more for each embedded
	// message the issue lies inside.
	Depth int
	// Detail is a short description for a log. It is built from numbers the
	// package read, never from text of the input.
	Detail string
}

func (i Issue) String() string {
	return fmt.Sprintf("%s at %d (depth %d): %s", i.Code, i.Offset, i.Depth, i.Detail)
}

// An Attribute is one attribute of the stream as it was read.
type Attribute struct {
	Level byte
	ID    uint32
	// Data is the attribute's data. It aliases the input.
	Data []byte
	// Offset is where in the input the attribute begins; its data begins nine
	// bytes later.
	Offset int64
	// Truncated reports that the stream ended inside Data.
	Truncated bool
	// BadChecksum reports that the checksum does not match Data.
	BadChecksum bool
}

// An Address is a sender or recipient.
type Address struct {
	Name    string
	Address string
	// Type is the address type, such as "SMTP" or "EX".
	Type string
}

// A Recipient is one row of the recipient table.
type Recipient struct {
	Address
	// Kind is PR_RECIPIENT_TYPE: 1 To, 2 Cc, 3 Bcc; zero when absent.
	Kind       int32
	Properties Properties
}

// A Message is what one TNEF stream holds.
//
// Byte slices in a Message, its attachments, attributes and properties alias
// the input given to Decode, apart from RTF when it was decompressed. Text
// fields are UTF-8; the bytes they were decoded from stay available through
// Attributes and Properties.
type Message struct {
	// Key is the stream's 16-bit key and Version its attTnefVersion.
	Key     uint16
	Version uint32

	Subject      string
	MessageClass string
	// MessageID is attMessageID, the sender's own identifier, and
	// InternetMessageID the PR_INTERNET_MESSAGE_ID property.
	MessageID         string
	InternetMessageID string
	From              Address
	// DateSent and DateReceived are zero when absent or not a date. Dates
	// read from attDateSent and attDateRecd carry no zone and are given as
	// UTC.
	DateSent     time.Time
	DateReceived time.Time

	// Body is the plain text body and BodyHTML the HTML body, each empty when
	// the stream has none.
	Body     string
	BodyHTML string
	// RTF is the PR_RTF_COMPRESSED body, decompressed, and RTFTruncated
	// reports that it is only the beginning of it.
	RTF          []byte
	RTFTruncated bool
	// HTMLFromRTF and TextFromRTF report that BodyHTML or Body was taken out
	// of RTF because the stream carries that body in no other form.
	HTMLFromRTF bool
	TextFromRTF bool

	// Codepage is the Windows code page 8-bit text was decoded from:
	// attOemCodepage, else PR_INTERNET_CPID, else PR_MESSAGE_CODEPAGE, else
	// for an embedded message the code page of the message it is in; zero
	// when none is declared. InternetCodepage is PR_INTERNET_CPID.
	Codepage         int
	InternetCodepage int

	Attachments []Attachment
	Recipients  []Recipient

	// Properties are the MAPI properties of attMAPIProps, in stream order.
	Properties Properties
	// Attributes are the message-level attributes, in stream order, with
	// those the package does not interpret.
	Attributes []Attribute
	// UnknownAttributes counts attributes whose identifier is not one
	// MS-OXTNEF defines. They were skipped; the message-level ones are in
	// Attributes.
	UnknownAttributes int

	// Incomplete reports that reading stopped before the end of the stream:
	// at a cut, at damage, or at a limit.
	Incomplete bool

	// Issues are what was repaired, skipped or left out, for this stream and
	// every message embedded in it. The messages in Attachment.Embedded have
	// none of their own.
	Issues []Issue
	// IssuesDropped counts issues past Limits.MaxIssues.
	IssuesDropped int
}

// The values of PR_ATTACH_METHOD.
const (
	AttachByValue     int32 = 1
	AttachByReference int32 = 2
	AttachEmbeddedMsg int32 = 5
	AttachOLE         int32 = 6
)

// AttachFlagMHTMLRef is the PR_ATTACH_FLAGS bit of an attachment the HTML body
// refers to.
const AttachFlagMHTMLRef uint32 = 0x4

// An Attachment is one attachment of a Message.
type Attachment struct {
	// Name is the short file name (PR_ATTACH_FILENAME, else attAttachTitle,
	// else attAttachTransportFilename) and LongName the long one
	// (PR_ATTACH_LONG_FILENAME). They are what the stream says, not safe
	// paths.
	Name        string
	LongName    string
	DisplayName string
	Extension   string
	MIMEType    string
	ContentID   string
	// ContentLocation is PR_ATTACH_CONTENT_LOCATION.
	ContentLocation string

	// Method is PR_ATTACH_METHOD and Flags PR_ATTACH_FLAGS, zero when absent.
	Method int32
	Flags  uint32
	// Hidden is PR_ATTACHMENT_HIDDEN. Inline reports that the attachment is
	// a resource of the body rather than a file for the reader: it is Hidden
	// or the HTML body refers to it (AttachFlagMHTMLRef).
	Hidden bool
	Inline bool

	// Data is the attachment's content: attAttachData, else
	// PR_ATTACH_DATA_BIN, else the object of PR_ATTACH_DATA_OBJ without its
	// interface identifier. Size is how many bytes the stream declares for
	// it and Truncated reports that Data is only the beginning of them.
	Data      []byte
	Size      int64
	Truncated bool
	// MetaFile is attAttachMetaFile, the rendering of the attachment's icon.
	MetaFile []byte

	Created  time.Time
	Modified time.Time

	// Embedded is the message the attachment carries when its content is a
	// TNEF stream of its own (PR_ATTACH_METHOD 5).
	Embedded *Message

	// Properties are the MAPI properties of attAttachment and Attributes the
	// attachment-level attributes, each in stream order.
	Properties Properties
	Attributes []Attribute
}

// Filename returns the name to show for the attachment: the long name, else
// the short one, else the display name. It is not a safe path.
func (a *Attachment) Filename() string {
	switch {
	case a.LongName != "":
		return a.LongName
	case a.Name != "":
		return a.Name
	}
	return a.DisplayName
}

// IsTNEF reports whether b begins with the TNEF signature.
func IsTNEF(b []byte) bool {
	return len(b) >= 4 && le32(b) == Signature
}

// Decode reads the TNEF stream in b. A nil lim is the defaults.
//
// It returns ErrNotTNEF, and no message, when b does not begin with the TNEF
// signature. Any other input yields a Message: what could be read, with
// Message.Issues saying what could not. The Message aliases b.
func Decode(b []byte, lim *Limits) (*Message, error) {
	if !IsTNEF(b) {
		return nil, ErrNotTNEF
	}
	d := &decoder{lim: lim.resolved()}
	cut := false
	if int64(len(b)) > d.lim.MaxInputBytes {
		b = b[:d.lim.MaxInputBytes]
		cut = true
		if len(b) < 4 {
			b = b[:0]
		}
	}
	d.left = d.lim.MaxDecodedBytes
	d.work = 4*int64(len(b)) + 4096
	if cut {
		d.issue(CodeLimitInput, int64(len(b)), 0, "input longer than MaxInputBytes; the rest is not read")
	}
	m := d.stream(b, 0, 0, 0)
	if cut {
		m.Incomplete = true
	}
	m.Issues = d.issues
	m.IssuesDropped = d.dropped
	return m, nil
}

// DecodeReader reads a TNEF stream from r, up to Limits.MaxInputBytes, and
// decodes it as Decode does. When r fails, its error is returned together
// with the message decoded from what was read before it.
func DecodeReader(r io.Reader, lim *Limits) (*Message, error) {
	max := lim.resolved().MaxInputBytes
	b, rerr := io.ReadAll(io.LimitReader(r, max+1))
	m, err := Decode(b, lim)
	if err != nil {
		if rerr != nil {
			return nil, rerr
		}
		return nil, err
	}
	return m, rerr
}

type decoder struct {
	lim     Limits
	attrs   int
	atts    int
	props   int
	left    int64 // decoded bytes still allowed
	work    int64 // checksum work resynchronisation may still spend
	issues  []Issue
	dropped int

	leftReported bool
}

func (d *decoder) issue(code Code, off int64, depth int, format string, args ...any) {
	if len(d.issues) >= d.lim.MaxIssues {
		d.dropped++
		return
	}
	detail := format
	if len(args) > 0 {
		detail = fmt.Sprintf(format, args...)
	}
	d.issues = append(d.issues, Issue{Code: code, Offset: off, Depth: depth, Detail: detail})
}

// grant takes up to n bytes from the decoded-bytes budget and returns how
// many it gave.
func (d *decoder) grant(n int, off int64, depth int) int {
	if int64(n) <= d.left {
		d.left -= int64(n)
		return n
	}
	n = int(d.left)
	d.left = 0
	if !d.leftReported {
		d.leftReported = true
		d.issue(CodeLimitDecodedBytes, off, depth, "MaxDecodedBytes reached; later content is cut")
	}
	return n
}

// raw is attribute data kept until the code page is known.
type raw struct {
	b   []byte
	off int64
	ok  bool
}

// attRaw is what was read of one attachment, beside its Attachment.
type attRaw struct {
	title, transport, data, meta raw
	created, modified            raw
	declared                     int64
	cut                          bool
	seen                         uint16
}

type state struct {
	m       *Message
	depth   int
	inherit int
	oem     uint32

	subject, body, class, msgid, from raw
	sent, recd                        raw

	atts   []attRaw // beside m.Attachments
	recips []Properties
	noAtts bool // attachment budget spent: later attachments are dropped

	cpReported bool
}

// stream reads one TNEF stream. cp is the code page of the message it is
// embedded in, which it takes when it declares none.
func (d *decoder) stream(b []byte, base int64, depth, cp int) *Message {
	m := &Message{}
	st := &state{m: m, depth: depth, inherit: cp}
	if len(b) < 6 {
		d.issue(CodeTruncated, base+int64(len(b)), depth, "stream ends inside its header")
		m.Incomplete = true
		return m
	}
	m.Key = le16(b[4:])
	pos := 6
	for pos < len(b) {
		at := base + int64(pos)
		if d.attrs >= d.lim.MaxAttributes {
			d.issue(CodeLimitAttributes, at, depth, "MaxAttributes reached; the rest of the stream is not read")
			m.Incomplete = true
			break
		}
		if len(b)-pos < 9 {
			d.issue(CodeTruncated, at, depth, "stream ends inside an attribute header (%d bytes left)", len(b)-pos)
			m.Incomplete = true
			break
		}
		level := b[pos]
		id := le32(b[pos+1:])
		n := le32(b[pos+5:])
		rest := b[pos+9 : len(b) : len(b)]
		cut := uint64(n) > uint64(len(rest))

		if level != LevelMessage && level != LevelAttachment {
			if d.candidate(b, pos, true) {
				d.issue(CodeLevel, at, depth, "level byte %#02x on attribute %#08x", level, id)
				level = LevelMessage
				if attachmentAttr(id) {
					level = LevelAttachment
				}
			} else if np := d.resync(b, pos+1); np >= 0 {
				d.issue(CodeResync, at, depth, "%d damaged bytes skipped", np-pos)
				pos = np
				continue
			} else {
				d.issue(CodeDamaged, at, depth, "level byte %#02x and no attribute after it", level)
				m.Incomplete = true
				break
			}
		} else if cut && !knownAttr(id) {
			// An unknown attribute that claims more than is left is more
			// likely damage than a cut: look for an attribute after it.
			if np := d.resync(b, pos+1); np >= 0 {
				d.issue(CodeResync, at, depth, "%d damaged bytes skipped", np-pos)
				pos = np
				continue
			}
		}

		d.attrs++
		a := Attribute{Level: level, ID: id, Offset: at}
		next := len(b)
		switch {
		case cut:
			a.Data = rest
			a.Truncated = true
			m.Incomplete = true
			d.issue(CodeTruncated, at, depth, "attribute %#08x declares %d bytes and %d are left", id, n, len(rest))
		case len(rest)-int(n) < 2:
			a.Data = rest[:n:n]
			m.Incomplete = true
			d.issue(CodeTruncated, at, depth, "stream ends before the checksum of attribute %#08x", id)
		default:
			a.Data = rest[:n:n]
			if checksum(a.Data) != le16(rest[n:]) {
				a.BadChecksum = true
				d.issue(CodeChecksum, at, depth, "checksum of attribute %#08x does not match", id)
			}
			next = pos + 9 + int(n) + 2
		}
		d.attribute(st, a, int64(n))
		pos = next
	}
	d.finish(st)
	return m
}

// candidate reports whether an attribute with a known identifier, a length
// that fits and a checksum that matches begins at b[i].
func (d *decoder) candidate(b []byte, i int, anyLevel bool) bool {
	if len(b)-i < 11 {
		return false
	}
	if l := b[i]; !anyLevel && l != LevelMessage && l != LevelAttachment {
		return false
	}
	if !knownAttr(le32(b[i+1:])) {
		return false
	}
	n := le32(b[i+5:])
	if uint64(n) > uint64(len(b)-i-11) {
		return false
	}
	cost := int64(n) + 16
	if cost > d.work {
		return false
	}
	d.work -= cost
	end := i + 9 + int(n)
	return checksum(b[i+9:end]) == le16(b[end:])
}

// resync returns the first position at or after from where an attribute
// verifies, or -1.
func (d *decoder) resync(b []byte, from int) int {
	for i := from; i+11 <= len(b); i++ {
		if d.candidate(b, i, false) {
			return i
		}
	}
	return -1
}

// The attribute slots of an attachment: a second attribute for a slot that is
// taken begins a new attachment, as a lost attAttachRendData would have.
const (
	slotTitle = 1 << iota
	slotData
	slotMeta
	slotCreated
	slotModified
	slotTransport
	slotProps
)

func slotOf(id uint32) uint16 {
	switch id {
	case AttAttachTitle:
		return slotTitle
	case AttAttachData:
		return slotData
	case AttAttachMetaFile:
		return slotMeta
	case AttAttachCreateDate:
		return slotCreated
	case AttAttachModifyDate:
		return slotModified
	case AttAttachTransportFilename:
		return slotTransport
	case AttAttachment:
		return slotProps
	}
	return 0
}

// attachment returns the index of the attachment the attribute id belongs to,
// beginning one when it has to, or -1 when the budget does not allow it.
func (d *decoder) attachment(st *state, id uint32, off int64) int {
	m := st.m
	cur := len(m.Attachments) - 1
	begin := id == AttAttachRendData
	if !begin {
		switch {
		case st.noAtts:
			return -1
		case cur < 0, st.atts[cur].seen&slotOf(id) != 0:
			begin = true
			d.issue(CodeNoRendData, off, st.depth, "attribute %#08x without attAttachRendData; an attachment is begun for it", id)
		}
	}
	if begin {
		if d.atts >= d.lim.MaxAttachments {
			if !st.noAtts {
				st.noAtts = true
				m.Incomplete = true
				d.issue(CodeLimitAttachments, off, st.depth, "MaxAttachments reached; later attachments are dropped")
			}
			return -1
		}
		d.atts++
		st.noAtts = false
		m.Attachments = append(m.Attachments, Attachment{})
		st.atts = append(st.atts, attRaw{})
		cur = len(m.Attachments) - 1
	}
	st.atts[cur].seen |= slotOf(id)
	return cur
}

func (d *decoder) attribute(st *state, a Attribute, declared int64) {
	m := st.m
	off := a.Offset + 9
	r := raw{b: a.Data, off: off, ok: true}
	known := knownAttr(a.ID)
	if !known {
		m.UnknownAttributes++
	} else if attachmentAttr(a.ID) != (a.Level == LevelAttachment) {
		d.issue(CodeLevel, a.Offset, st.depth, "level byte %#02x on attribute %#08x", a.Level, a.ID)
	}

	if attachmentAttr(a.ID) {
		i := d.attachment(st, a.ID, a.Offset)
		if i < 0 {
			return
		}
		att, ar := &m.Attachments[i], &st.atts[i]
		att.Attributes = append(att.Attributes, a)
		switch a.ID {
		case AttAttachTitle:
			ar.title = r
		case AttAttachTransportFilename:
			ar.transport = r
		case AttAttachData:
			ar.data, ar.declared, ar.cut = r, declared, a.Truncated
		case AttAttachMetaFile:
			ar.meta = r
		case AttAttachCreateDate:
			ar.created = r
		case AttAttachModifyDate:
			ar.modified = r
		case AttAttachment:
			c := cursor{b: a.Data}
			ps, _ := d.properties(&c, off, st.depth)
			att.Properties = append(att.Properties, ps...)
		}
		return
	}
	if !known && a.Level == LevelAttachment && len(m.Attachments) > 0 && !st.noAtts {
		att := &m.Attachments[len(m.Attachments)-1]
		att.Attributes = append(att.Attributes, a)
		return
	}

	m.Attributes = append(m.Attributes, a)
	switch a.ID {
	case AttTnefVersion:
		if len(a.Data) >= 4 {
			m.Version = le32(a.Data)
		}
	case AttOemCodepage:
		if len(a.Data) >= 4 {
			st.oem = le32(a.Data)
		}
	case AttSubject:
		st.subject = r
	case AttBody:
		st.body = r
	case AttMessageClass:
		st.class = r
	case AttMessageID:
		st.msgid = r
	case AttFrom:
		st.from = r
	case AttDateSent:
		st.sent = r
	case AttDateRecd:
		st.recd = r
	case AttMAPIProps:
		c := cursor{b: a.Data}
		ps, _ := d.properties(&c, off, st.depth)
		m.Properties = append(m.Properties, ps...)
	case AttRecipTable:
		d.recipients(st, a.Data, off)
	}
}

func (d *decoder) recipients(st *state, data []byte, off int64) {
	c := cursor{b: data}
	rows, ok := c.u32()
	if !ok {
		d.issue(CodePropertyTruncated, off, st.depth, "recipient table ends inside its row count")
		return
	}
	for i := uint32(0); i < rows; i++ {
		at := off + int64(c.pos)
		if c.left() < 4 {
			d.issue(CodePropertyTruncated, at, st.depth, "recipient table declares %d rows and ends after %d", rows, i)
			return
		}
		if d.props >= d.lim.MaxTotalProperties {
			d.issue(CodeLimitProperties, at, st.depth, "MaxTotalProperties reached; later recipients are dropped")
			return
		}
		d.props++
		ps, ok := d.properties(&c, off, st.depth)
		st.recips = append(st.recips, ps)
		if !ok {
			return
		}
	}
}

func cpOf(v uint32) int {
	if v > 0xFFFF {
		return -1
	}
	return int(v)
}

func (d *decoder) finish(st *state) {
	m := st.m
	ps := m.Properties

	if v, ok := ps.long(PropInternetCPID); ok {
		m.InternetCodepage = cpOf(v)
	}
	switch {
	case st.oem != 0:
		m.Codepage = cpOf(st.oem)
	case m.InternetCodepage != 0:
		m.Codepage = m.InternetCodepage
	default:
		if v, ok := ps.long(PropMessageCodepage); ok {
			m.Codepage = cpOf(v)
		}
	}
	if m.Codepage == 0 {
		m.Codepage = st.inherit
	}

	short, long := d.lim.MaxStringBytes, d.lim.MaxBodyBytes
	m.Subject = d.pick(st, ps, PropSubject, st.subject, short)
	m.MessageClass = d.pick(st, ps, PropMessageClass, st.class, short)
	m.MessageID = d.pick(st, nil, 0, st.msgid, short)
	m.InternetMessageID = d.pick(st, ps, PropInternetMessageID, raw{}, short)
	m.Body = d.pick(st, ps, PropBody, st.body, long)

	m.From.Name = d.pick(st, ps, PropSenderName, raw{}, short)
	m.From.Type = d.pick(st, ps, PropSenderAddrType, raw{}, short)
	m.From.Address = d.pick(st, ps, PropSenderSMTPAddress, raw{}, short)
	if m.From.Address == "" {
		m.From.Address = d.pick(st, ps, PropSenderEmailAddress, raw{}, short)
	}
	if m.From == (Address{}) && st.from.ok {
		m.From = d.trp(st, st.from)
	}

	if t, ok := ps.time(PropClientSubmitTime); ok {
		m.DateSent = t
	} else if st.sent.ok {
		m.DateSent = dtr(st.sent.b)
	}
	if t, ok := ps.time(PropMessageDeliveryTime); ok {
		m.DateReceived = t
	} else if st.recd.ok {
		m.DateReceived = dtr(st.recd.b)
	}

	if p := ps.Find(PropBodyHTML); p != nil {
		switch p.Type {
		case TypeBinary:
			m.BodyHTML = d.html(st, p)
		case TypeString8, TypeUnicode:
			m.BodyHTML = d.text(st, p.Value, p.Type, p.Offset, long, CodeLimitBodyBytes)
		}
	}
	if p := ps.Find(PropRTFCompressed); p != nil && p.Type == TypeBinary {
		d.rtf(st, p)
	}

	for i, row := range st.recips {
		r := Recipient{Properties: row}
		r.Name = d.pick(st, row, PropDisplayName, raw{}, short)
		r.Type = d.pick(st, row, PropAddrType, raw{}, short)
		r.Address.Address = d.pick(st, row, PropSMTPAddress, raw{}, short)
		if r.Address.Address == "" {
			r.Address.Address = d.pick(st, row, PropEmailAddress, raw{}, short)
		}
		if v, ok := row.long(PropRecipientType); ok {
			r.Kind = int32(v)
		}
		if i == 0 {
			m.Recipients = make([]Recipient, 0, len(st.recips))
		}
		m.Recipients = append(m.Recipients, r)
	}

	for i := range m.Attachments {
		d.finishAttachment(st, &m.Attachments[i], &st.atts[i])
	}
}

// html decodes a binary PR_BODY_HTML: it is in the PR_INTERNET_CPID code
// page, else the stream's.
func (d *decoder) html(st *state, p *Property) string {
	b := p.Value
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	cp := st.m.InternetCodepage
	if cp == 0 {
		cp = st.m.Codepage
	}
	if int64(len(b)) > d.lim.MaxBodyBytes {
		b = b[:d.lim.MaxBodyBytes]
		d.issue(CodeLimitBodyBytes, p.Offset, st.depth, "HTML body longer than MaxBodyBytes; cut")
	}
	if int64(len(b)) > d.left {
		b = b[:d.grant(len(b), p.Offset, st.depth)]
		d.left += int64(len(b))
	}
	s, ok := DecodeText(b, cp)
	if !ok {
		d.codepage(st, cp, p.Offset)
	}
	return d.charge(s, p.Offset, st.depth)
}

func (d *decoder) rtf(st *state, p *Property) {
	m := st.m
	max := d.lim.MaxRTFBytes
	if d.left < max {
		max = d.left
	}
	out, status := decompressRTF(p.Value, int(max))
	if status&rtfUnknown != 0 {
		d.issue(CodeRTFCompression, p.Offset, st.depth, "compressed RTF of an unknown compression type")
		return
	}
	if status&rtfBadCRC != 0 {
		d.issue(CodeRTFCRC, p.Offset, st.depth, "compressed RTF fails its CRC")
	}
	if status&(rtfShort|rtfCut) != 0 || p.Truncated {
		m.RTFTruncated = true
		d.issue(CodeRTFDamaged, p.Offset, st.depth, "compressed RTF is cut short")
	}
	d.left -= int64(len(out))
	if status&rtfLimit != 0 {
		m.RTFTruncated = true
		if max < d.lim.MaxRTFBytes {
			d.grant(1, p.Offset, st.depth) // nothing is left: reports it
		} else {
			d.issue(CodeLimitRTFBytes, p.Offset, st.depth, "compressed RTF decompresses to more than MaxRTFBytes; cut")
		}
	}
	m.RTF = out
	if len(out) == 0 || (m.Body != "" && m.BodyHTML != "") {
		return
	}
	max = d.lim.MaxBodyBytes
	if d.left < max {
		max = d.left
	}
	kind, text, complete := ExtractRTF(out, m.Codepage, int(max))
	switch {
	case kind == RTFHTML && m.BodyHTML == "":
		m.BodyHTML, m.HTMLFromRTF = text, text != ""
	case kind != RTFHTML && m.Body == "":
		m.Body, m.TextFromRTF = text, text != ""
	default:
		return
	}
	d.left -= int64(len(text))
	if !complete {
		d.issue(CodeRTFExtract, p.Offset, st.depth, "the content of the RTF body was extracted only in part")
	}
}

func (d *decoder) finishAttachment(st *state, a *Attachment, ar *attRaw) {
	ps := a.Properties
	short := d.lim.MaxStringBytes
	a.LongName = d.pick(st, ps, PropAttachLongFilename, raw{}, short)
	a.Name = d.pick(st, ps, PropAttachFilename, ar.title, short)
	if a.Name == "" {
		a.Name = d.pick(st, nil, 0, ar.transport, short)
	}
	a.DisplayName = d.pick(st, ps, PropDisplayName, raw{}, short)
	a.Extension = d.pick(st, ps, PropAttachExtension, raw{}, short)
	a.MIMEType = d.pick(st, ps, PropAttachMIMETag, raw{}, short)
	a.ContentID = d.pick(st, ps, PropAttachContentID, raw{}, short)
	a.ContentLocation = d.pick(st, ps, PropAttachContentLocation, raw{}, short)
	if v, ok := ps.long(PropAttachMethod); ok {
		a.Method = int32(v)
	}
	if v, ok := ps.long(PropAttachFlags); ok {
		a.Flags = v
	}
	if p := ps.Find(PropAttachmentHidden); p != nil {
		a.Hidden, _ = p.Bool()
	}
	a.Inline = a.Hidden || a.Flags&AttachFlagMHTMLRef != 0

	if t, ok := ps.time(PropCreationTime); ok {
		a.Created = t
	} else if ar.created.ok {
		a.Created = dtr(ar.created.b)
	}
	if t, ok := ps.time(PropLastModificationTime); ok {
		a.Modified = t
	} else if ar.modified.ok {
		a.Modified = dtr(ar.modified.b)
	}

	// The content: the attribute, else the property.
	var payload raw
	object := false
	if p := ps.Find(PropAttachData); p != nil {
		switch p.Type {
		case TypeBinary:
			payload = raw{b: p.Value, off: p.Offset, ok: true}
		case TypeObject:
			// An object begins with the identifier of its interface.
			object = true
			if len(p.Value) >= 16 {
				payload = raw{b: p.Value[16:], off: p.Offset + 16, ok: true}
			}
		}
		if payload.ok && !ar.data.ok {
			a.Truncated = p.Truncated
		}
	}
	content := payload
	if ar.data.ok {
		content = ar.data
		a.Size, a.Truncated = ar.declared, ar.cut
	} else {
		a.Size = int64(len(payload.b))
	}

	if object || a.Method == AttachEmbeddedMsg {
		src := payload
		if !src.ok {
			src = ar.data
		}
		if !IsTNEF(src.b) && len(src.b) > 16 && IsTNEF(src.b[16:]) {
			src.b, src.off = src.b[16:], src.off+16
		}
		switch {
		case !IsTNEF(src.b):
			if a.Method == AttachEmbeddedMsg {
				d.issue(CodeEmbeddedNotTNEF, src.off, st.depth, "attachment declares an embedded message and holds no TNEF stream")
			}
		case st.depth >= d.lim.MaxDepth:
			d.issue(CodeLimitDepth, src.off, st.depth, "embedded message deeper than MaxDepth; not read")
		default:
			a.Embedded = d.stream(src.b, src.off, st.depth+1, st.m.Codepage)
		}
	}

	a.Data = d.bytes(st, content, &a.Truncated)
	if ar.meta.ok {
		var cut bool
		a.MetaFile = d.bytes(st, ar.meta, &cut)
	}
}

// bytes applies the attachment and decoded-bytes budgets to content.
func (d *decoder) bytes(st *state, r raw, cut *bool) []byte {
	b := r.b
	if int64(len(b)) > d.lim.MaxAttachmentBytes {
		b = b[:d.lim.MaxAttachmentBytes]
		*cut = true
		d.issue(CodeLimitAttachmentBytes, r.off, st.depth, "attachment longer than MaxAttachmentBytes; cut")
	}
	if n := d.grant(len(b), r.off, st.depth); n < len(b) {
		b = b[:n]
		*cut = true
	}
	return b[:len(b):len(b)]
}

// trp reads attFrom: a TRP structure, then the display name, then the
// address as "TYPE:address".
func (d *decoder) trp(st *state, r raw) Address {
	var a Address
	b := r.b
	if len(b) < 8 {
		return a
	}
	cch, cb := int(le16(b[4:])), int(le16(b[6:]))
	b = b[8:]
	if cch > len(b) {
		cch = len(b)
	}
	short := d.lim.MaxStringBytes
	a.Name = d.text(st, b[:cch], TypeString8, r.off+8, short, CodeLimitStringBytes)
	b = b[cch:]
	if cb > len(b) {
		cb = len(b)
	}
	addr := d.text(st, b[:cb], TypeString8, r.off+8+int64(cch), short, CodeLimitStringBytes)
	for i := 0; i < len(addr) && i < 16; i++ {
		if addr[i] == ':' {
			a.Type, addr = addr[:i], addr[i+1:]
			break
		}
	}
	a.Address = addr
	return a
}

// pick returns the text of property id, else of the attribute data att.
func (d *decoder) pick(st *state, ps Properties, id uint16, att raw, max int64) string {
	code := CodeLimitStringBytes
	if max == d.lim.MaxBodyBytes {
		code = CodeLimitBodyBytes
	}
	if id != 0 {
		var narrow *Property
		for i := range ps {
			p := &ps[i]
			if p.ID != id {
				continue
			}
			if p.Type == TypeUnicode {
				return d.text(st, p.Value, TypeUnicode, p.Offset, max, code)
			}
			if p.Type == TypeString8 && narrow == nil {
				narrow = p
			}
		}
		if narrow != nil {
			return d.text(st, narrow.Value, TypeString8, narrow.Offset, max, code)
		}
	}
	if att.ok {
		return d.text(st, att.b, TypeString8, att.off, max, code)
	}
	return ""
}

// text decodes one string value under the string and decoded-bytes budgets.
func (d *decoder) text(st *state, b []byte, typ uint16, off int64, max int64, code Code) string {
	b = terminated(b, typ == TypeUnicode)
	if int64(len(b)) > max {
		b = b[:max]
		d.issue(code, off, st.depth, "text longer than its limit; cut")
	}
	if int64(len(b)) > d.left {
		b = b[:d.grant(len(b), off, st.depth)]
		d.left += int64(len(b))
	}
	if len(b) == 0 {
		return ""
	}
	var s string
	if typ == TypeUnicode {
		s = decodeUTF16(b)
	} else {
		var ok bool
		if s, ok = DecodeText(b, st.m.Codepage); !ok {
			d.codepage(st, st.m.Codepage, off)
		}
	}
	return d.charge(s, off, st.depth)
}

// charge takes s from the decoded-bytes budget, cutting it to what is left.
func (d *decoder) charge(s string, off int64, depth int) string {
	if n := d.grant(len(s), off, depth); n < len(s) {
		for n > 0 && s[n]&0xC0 == 0x80 {
			n--
		}
		s = s[:n]
	}
	return s
}

func (d *decoder) codepage(st *state, cp int, off int64) {
	if st.cpReported {
		return
	}
	st.cpReported = true
	if cp == 0 {
		d.issue(CodeCodepageUndeclared, off, st.depth, "non-ASCII text and no code page declared; read as UTF-8 or Windows-1252")
		return
	}
	d.issue(CodeCodepageUnknown, off, st.depth, "code page %d is not supported; text read as UTF-8 or Windows-1252", cp)
}

// terminated cuts b at its first NUL character.
func terminated(b []byte, wide bool) []byte {
	if wide {
		b = b[:len(b)&^1]
		for i := 0; i < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				return b[:i]
			}
		}
		return b
	}
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// dtr reads a DTR structure: year, month, day, hour, minute, second and day
// of week, 16 bits each.
func dtr(b []byte) time.Time {
	if len(b) < 12 {
		return time.Time{}
	}
	y, mo, da := int(le16(b)), int(le16(b[2:])), int(le16(b[4:]))
	h, mi, s := int(le16(b[6:])), int(le16(b[8:])), int(le16(b[10:]))
	if y == 0 || y > 9999 || mo < 1 || mo > 12 || da < 1 || da > 31 || h > 23 || mi > 59 || s > 59 {
		return time.Time{}
	}
	return time.Date(y, time.Month(mo), da, h, mi, s, 0, time.UTC)
}

// filetime converts 100 ns intervals since 1601 to a time.
func filetime(v uint64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(int64(v/10000000)-11644473600, int64(v%10000000)*100).UTC()
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 { return uint64(le32(b)) | uint64(le32(b[4:]))<<32 }

// checksum is the 16-bit sum of the bytes of b.
func checksum(b []byte) uint16 {
	var s uint16
	for _, c := range b {
		s += uint16(c)
	}
	return s
}
