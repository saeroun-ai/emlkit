package pgpmime

import "fmt"

// A Code names what an Issue is about. Program logic should branch on the
// Code, never on the Message.
type Code string

// Structure and classification codes.
const (
	// CodePartCount: a multipart/signed or multipart/encrypted does not have
	// exactly two parts, or a rewrapped message has parts beside its two.
	CodePartCount Code = "structure.part_count"
	// CodePartOrder: the parts are present in the wrong order.
	CodePartOrder Code = "structure.part_order"
	// CodeProtocolMissing: the multipart declares no protocol, and its parts
	// show it is PGP/MIME.
	CodeProtocolMissing Code = "structure.protocol_missing"
	// CodeProtocolMismatch: the declared protocol and the parts disagree.
	CodeProtocolMismatch Code = "structure.protocol_mismatch"
	// CodeMicalgMissing: a PGP/MIME signed message declares no micalg.
	CodeMicalgMissing Code = "structure.micalg_missing"
	// CodeMicalgInvalid: the micalg is not a pgp- hash name.
	CodeMicalgInvalid Code = "structure.micalg_invalid"
	// CodeControlMissing: a PGP/MIME encrypted message has no
	// application/pgp-encrypted control part.
	CodeControlMissing Code = "structure.control_missing"
	// CodeControlVersion: the control part does not say "Version: 1".
	CodeControlVersion Code = "structure.control_version"
	// CodePayloadType: the encrypted payload is not application/octet-stream.
	CodePayloadType Code = "structure.payload_type"
	// CodeRewrapped: a multipart/mixed carries the two parts of a PGP/MIME
	// encrypted message as attachments, as Microsoft Exchange rewrites it.
	CodeRewrapped Code = "structure.rewrapped"
	// CodeTypeMismatch: a part's content does not match its declared type or
	// its file name.
	CodeTypeMismatch Code = "classify.type_mismatch"
)

// Armor codes. Offsets are of the input given to Parse.
const (
	// CodeArmorQuoted: the armor lines carry a quoting or indentation prefix,
	// which was removed. Informational.
	CodeArmorQuoted Code = "armor.quoted"
	// CodeArmorLabelUnknown: a BEGIN PGP line names no known block type.
	CodeArmorLabelUnknown Code = "armor.label_unknown"
	// CodeArmorBeginDamaged: the BEGIN line is not exactly five dashes, the
	// label and five dashes.
	CodeArmorBeginDamaged Code = "armor.begin_damaged"
	// CodeArmorHeaderInvalid: an armor header line is malformed or over the
	// limit, and was skipped.
	CodeArmorHeaderInvalid Code = "armor.header_invalid"
	// CodeArmorBlankLineMissing: the armor headers are not followed by an
	// empty line; the body was taken to start at the first line that is not
	// a header.
	CodeArmorBlankLineMissing Code = "armor.blank_line_missing"
	// CodeArmorEndMissing: the block has no END line; it was taken to end at
	// the next BEGIN line, the first line that is not base64, or the end of
	// the input.
	CodeArmorEndMissing Code = "armor.end_missing"
	// CodeArmorEndMismatch: the END line names another block type.
	CodeArmorEndMismatch Code = "armor.end_mismatch"
	// CodeArmorEndDamaged: the END line was recognised although its dashes
	// are damaged.
	CodeArmorEndDamaged Code = "armor.end_damaged"
	// CodeArmorInvalidCharacter: body lines hold characters outside base64;
	// they were skipped.
	CodeArmorInvalidCharacter Code = "armor.invalid_character"
	// CodeArmorPadding: base64 padding is misplaced, or the body ends in an
	// incomplete group; the data was decoded around it.
	CodeArmorPadding Code = "armor.padding"
	// CodeArmorDataAfterCRC: body lines follow the checksum line; they were
	// ignored.
	CodeArmorDataAfterCRC Code = "armor.data_after_crc"
	// CodeArmorEmpty: the block decodes to no data.
	CodeArmorEmpty Code = "armor.empty"
	// CodeCRCMissing: the block has no checksum line. RFC 9580 makes the
	// checksum optional, so this is informational.
	CodeCRCMissing Code = "armor.crc_missing"
	// CodeCRCMismatch: the checksum does not match the data. The data is
	// returned as decoded.
	CodeCRCMismatch Code = "armor.crc_mismatch"
	// CodeCRCInvalid: the checksum line is not "=" and four base64 characters.
	CodeCRCInvalid Code = "armor.crc_invalid"
)

// Cleartext signature codes.
const (
	// CodeSignatureMissing: a cleartext signed message has no signature
	// block after its text.
	CodeSignatureMissing Code = "cleartext.signature_missing"
	// CodeUnescapedDash: a line of the signed text starts with a dash that is
	// not dash-escaped; it was kept as it is.
	CodeUnescapedDash Code = "cleartext.unescaped_dash"
	// CodeHashUnknown: a Hash header names no known hash algorithm.
	CodeHashUnknown Code = "cleartext.hash_unknown"
	// CodeHashMismatch: the hash of the signature is not one the Hash header
	// named.
	CodeHashMismatch Code = "cleartext.hash_mismatch"
)

// Packet codes. Offsets are of the packet stream being walked: a block's
// Data, the binary input, or the content of a compressed packet.
const (
	// CodePacketInvalidHeader: a byte where a packet header should start is
	// not one; the rest of the stream was not read.
	CodePacketInvalidHeader Code = "packet.invalid_header"
	// CodePacketTruncated: the stream ends inside a packet header or body.
	CodePacketTruncated Code = "packet.truncated"
	// CodePacketPartialNotAllowed: a packet other than a data packet uses
	// partial body lengths; its body was not decoded.
	CodePacketPartialNotAllowed Code = "packet.partial_not_allowed"
	// CodePacketPartialShort: the first partial body chunk is shorter than
	// the 512 octets RFC 4880 requires. Informational.
	CodePacketPartialShort Code = "packet.partial_short"
	// CodePacketUnknownTag: a packet tag is reserved, private or unknown.
	CodePacketUnknownTag Code = "packet.unknown_tag"
	// CodePacketVersion: a known packet has a version this package does not
	// decode.
	CodePacketVersion Code = "packet.version_unknown"
	// CodePacketMalformed: a packet body is shorter than its fields, or
	// inconsistent.
	CodePacketMalformed Code = "packet.malformed"
	// CodeLegacyEncryption: data is encrypted without integrity protection
	// (Symmetrically Encrypted Data, tag 9).
	CodeLegacyEncryption Code = "packet.legacy_encryption"
	// CodeCompressionUnknown: a compressed packet uses an unknown algorithm.
	CodeCompressionUnknown Code = "packet.compression_unknown"
	// CodeDecompressError: compressed data is corrupt; what inflated before
	// the error was walked.
	CodeDecompressError Code = "packet.decompress_error"
)

// Budget codes: a Limits value was reached and the rest was skipped or cut.
const (
	CodeInputLimit        Code = "limit.input"
	CodeBlockLimit        Code = "limit.blocks"
	CodePacketLimit       Code = "limit.packets"
	CodeKeptLimit         Code = "limit.kept_bytes"
	CodeDecompressLimit   Code = "limit.decompressed_bytes"
	CodeDepthLimit        Code = "limit.depth"
	CodeSubpacketLimit    Code = "limit.subpackets"
	CodeUserIDLimit       Code = "limit.user_ids"
	CodeArmorHeaderLimit  Code = "limit.armor_headers"
	CodeIssueLimit        Code = "limit.issues"
	CodeVerifyNoSigned    Code = "verify.no_signed_bytes"
	CodeVerifyNoSignature Code = "verify.no_signature"
	CodeVerifySigType     Code = "verify.signature_type"
	CodeMicalgMismatch    Code = "verify.micalg_mismatch"
	CodeNotEncrypted      Code = "decrypt.not_encrypted"
)

// An Issue is something found wrong, unusual or cut short. Offset is a byte
// offset in the data the issue's Code family refers to (see the Code
// constants), or -1 when it has no place.
type Issue struct {
	Code    Code
	Offset  int64
	Message string
}

func (i Issue) String() string {
	if i.Offset < 0 {
		return string(i.Code) + ": " + i.Message
	}
	return fmt.Sprintf("%s at %d: %s", i.Code, i.Offset, i.Message)
}

// HasIssue reports whether any of issues has the code.
func HasIssue(issues []Issue, code Code) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}
