package pgpmime

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

func vector(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func codes(is []Issue) []Code {
	var out []Code
	for _, i := range is {
		if !slices.Contains(out, i.Code) {
			out = append(out, i.Code)
		}
	}
	slices.Sort(out)
	return out
}

func sorted(cs ...Code) []Code {
	slices.Sort(cs)
	return cs
}

func eachLine(s, prefix, suffix string) string {
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	for _, l := range lines {
		if l == "" {
			continue
		}
		b.WriteString(prefix + strings.TrimSuffix(l, "\n") + suffix + "\n")
	}
	return b.String()
}

func TestArmorDamage(t *testing.T) {
	base := vector(t, "encrypted.asc")
	want := Parse([]byte(base), nil).Blocks[0].Data
	r := strings.NewReplacer
	noEnd := strings.Replace(base, "-----END PGP MESSAGE-----\n", "", 1)
	tests := []struct {
		name  string
		in    string
		codes []Code
	}{
		{"as made", base, nil},
		{"CRLF", strings.ReplaceAll(base, "\n", "\r\n"), nil},
		{"CR only", strings.ReplaceAll(base, "\n", "\r"), nil},
		{"quoted", eachLine(base, "> ", ""), sorted(CodeArmorQuoted)},
		{"quoted twice", eachLine(base, "> > ", ""), sorted(CodeArmorQuoted)},
		{"indented", eachLine(base, "    ", ""), sorted(CodeArmorQuoted)},
		{"trailing white space", eachLine(base, "", " \t "), nil},
		{"headers", r("-----\n\n", "-----\nVersion: GnuPG v2\nComment: hi: there\n\n").Replace(base), nil},
		{"no blank line", r("-----\n\n", "-----\nComment: x\n").Replace(base), sorted(CodeArmorBlankLineMissing)},
		{"no blank line nor header", r("-----\n\n", "-----\n").Replace(base), sorted(CodeArmorBlankLineMissing)},
		{"no END", noEnd, sorted(CodeArmorEndMissing)},
		{"no END, then prose", noEnd + "Thanks, Bob\n", sorted(CodeArmorEndMissing)},
		{"no END nor CRC, then prose", strings.Replace(noEnd, "=+fON\n", "", 1) + "Thanks, Bob\n",
			sorted(CodeArmorEndMissing, CodeCRCMissing)},
		{"damaged END", r("-----END PGP MESSAGE-----", "----END PGP MESSAGE---").Replace(base), sorted(CodeArmorEndDamaged)},
		{"END mismatch", r("-----END PGP MESSAGE-----", "-----END PGP SIGNATURE-----").Replace(base), sorted(CodeArmorEndMismatch)},
		{"CRC mismatch", r("=+fON", "=AAAA").Replace(base), sorted(CodeCRCMismatch)},
		{"CRC missing", r("=+fON\n", "").Replace(base), sorted(CodeCRCMissing)},
		{"CRC malformed", r("=+fON", "=+f").Replace(base), sorted(CodeCRCInvalid)},
		{"stray characters", r("Pz8w\n", "Pz8w!\n", "30Tl\n", "30T l\n").Replace(base), sorted(CodeArmorInvalidCharacter)},
		{"padding lost", r("Gw==\n", "Gw\n").Replace(base), nil},
		{"lines rejoined", r("8w\nNg", "8wNg", "Tl\n0r", "Tl0r").Replace(base), nil},
		{"data after CRC", r("=+fON\n", "=+fON\nAAAA\n").Replace(base), sorted(CodeArmorDataAfterCRC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Parse([]byte(tt.in), nil)
			if len(res.Blocks) != 1 {
				t.Fatalf("%d blocks", len(res.Blocks))
			}
			b := res.Blocks[0]
			if got := codes(b.Issues); !slices.Equal(got, tt.codes) {
				t.Errorf("issues %v, want %v", b.Issues, tt.codes)
			}
			if !bytes.Equal(b.Data, want) {
				t.Errorf("data differs: %d bytes, want %d", len(b.Data), len(want))
			}
			if got := tags(b.Packets); got != "pkesk seipd" {
				t.Errorf("packets %q", got)
			}
			if b.Start < 0 || b.End > int64(len(tt.in)) || b.Start >= b.End {
				t.Errorf("range [%d, %d) of %d", b.Start, b.End, len(tt.in))
			}
		})
	}
}

func tags(ps []Packet) string {
	var out []string
	for _, p := range ps {
		out = append(out, strings.Repeat(">", p.Depth)+p.Tag.String())
	}
	return strings.Join(out, " ")
}

func TestArmorBlocks(t *testing.T) {
	msg, key := vector(t, "encrypted.asc"), vector(t, "pubkey.asc")
	noEnd := strings.Replace(msg, "-----END PGP MESSAGE-----\n", "", 1)
	// The first half of the message's base64, as a split message's first part.
	half := strings.Join(strings.Split(msg, "\n")[2:5], "\n")
	tests := []struct {
		name  string
		in    string
		types []ArmorType
		codes [][]Code
	}{
		{"among prose", "Hi,\n\n" + msg + "\nmy key:\n" + key + "bye\n",
			[]ArmorType{ArmorMessage, ArmorPublicKey}, [][]Code{nil, nil}},
		{"END missing before the next block", noEnd + key,
			[]ArmorType{ArmorMessage, ArmorPublicKey}, [][]Code{sorted(CodeArmorEndMissing), nil}},
		{"split message", "-----BEGIN PGP MESSAGE, PART 1/2-----\n\n" + half + "\n-----END PGP MESSAGE, PART 1/2-----\n",
			[]ArmorType{ArmorMessagePart}, [][]Code{sorted(CodeCRCMissing, CodePacketTruncated)}},
		{"unknown label", "-----BEGIN PGP ARMORED FILE-----\n\nAAAA\n-----END PGP ARMORED FILE-----\n",
			[]ArmorType{ArmorUnknown}, [][]Code{sorted(CodeArmorLabelUnknown, CodeCRCMissing, CodePacketInvalidHeader)}},
		{"empty", "-----BEGIN PGP MESSAGE-----\n\n-----END PGP MESSAGE-----\n",
			[]ArmorType{ArmorMessage}, [][]Code{sorted(CodeArmorEmpty)}},
		{"damaged BEGIN", "---BEGIN PGP MESSAGE-----\n" + strings.SplitN(msg, "\n", 2)[1],
			[]ArmorType{ArmorMessage}, [][]Code{sorted(CodeArmorBeginDamaged)}},
		{"byte order mark", "\xEF\xBB\xBF" + key, []ArmorType{ArmorPublicKey}, [][]Code{nil}},
		{"not armor", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Parse([]byte(tt.in), nil)
			var got []ArmorType
			for i, b := range res.Blocks {
				got = append(got, b.Type)
				if i < len(tt.codes) && !slices.Equal(codes(b.Issues), tt.codes[i]) {
					t.Errorf("block %d issues %v, want %v", i, b.Issues, tt.codes[i])
				}
				if !strings.HasPrefix(strings.TrimPrefix(tt.in[b.Start:], "\xEF\xBB\xBF"), "-") && !strings.HasPrefix(tt.in[b.Start:], ">") {
					t.Errorf("block %d starts at %q", i, tt.in[b.Start:min(int(b.Start)+10, len(tt.in))])
				}
			}
			if !slices.Equal(got, tt.types) {
				t.Errorf("types %v, want %v", got, tt.types)
			}
		})
	}
	if b := Parse([]byte("-----BEGIN PGP MESSAGE, PART 2/3-----\n\nAAAA\n-----END PGP MESSAGE, PART 2/3-----\n"), nil).Blocks[0]; b.Part != 2 || b.Total != 3 || b.Packets != nil {
		t.Errorf("part block %+v", b)
	}
}

func TestCRC24(t *testing.T) {
	if got := crc24(nil); got != 0xB704CE {
		t.Errorf("crc24(nil) = %06X", got)
	}
	if got := crc24([]byte("123456789")); got != 0x21CF02 {
		t.Errorf("crc24(check) = %06X", got)
	}
}

func TestDecodeArmorBase64(t *testing.T) {
	for _, tt := range []struct {
		in, out string
		damaged bool
	}{
		{"QUJD", "ABC", false},
		{"QUI=", "AB", false},
		{"QUI", "AB", false},
		{"QQ==QUI=", "AAB", true},
		{"QUJDR", "ABC", true},
		{"", "", false},
	} {
		out, damaged := decodeArmorBase64([]byte(tt.in))
		if string(out) != tt.out || damaged != tt.damaged {
			t.Errorf("%q: %q %v", tt.in, out, damaged)
		}
	}
}

func TestCleartext(t *testing.T) {
	sig := vector(t, "detached.asc")
	const begin = "-----BEGIN PGP SIGNED MESSAGE-----\n"
	tests := []struct {
		name      string
		in        string
		hash      []string
		text      string
		canonical string
		codes     []Code
		signed    bool
	}{
		{"plain", begin + "Hash: SHA256\n\nline one  \n- -dash\n- From me\n\n" + sig,
			[]string{"SHA256"}, "line one  \n-dash\nFrom me\n", "line one\r\n-dash\r\nFrom me\r\n", nil, true},
		{"no Hash header", begin + "\nhello\n" + sig, nil, "hello", "hello", nil, true},
		{"two hashes", begin + "Hash: SHA1, SHA256\n\nx\n" + sig, []string{"SHA1", "SHA256"}, "x", "x", nil, true},
		{"unknown hash", begin + "Hash: FOO\n\nx\n" + sig, []string{"FOO"}, "x", "x", sorted(CodeHashUnknown), true},
		{"unescaped dash", begin + "\n-oops\n" + sig, nil, "-oops", "-oops", sorted(CodeUnescapedDash), true},
		{"no blank line", begin + "Hash: SHA256\nHello: world\n" + sig, []string{"SHA256"}, "Hello: world", "Hello: world",
			sorted(CodeArmorBlankLineMissing), true},
		{"quoted", eachLine(begin+"Hash: SHA256\n\na \n\n- -b\n"+sig, "> ", ""), []string{"SHA256"}, "a \n\n-b", "a\r\n\r\n-b",
			sorted(CodeArmorQuoted), true},
		{"quoted, empty lines bare", strings.ReplaceAll(eachLine(begin+"\na\n\nb\n"+sig, "> ", ""), "\n> \n", "\n>\n"), nil, "a\n\nb", "a\r\n\r\nb",
			sorted(CodeArmorQuoted), true},
		{"CRLF and tabs", strings.ReplaceAll(begin+"\na\t\nb\n"+sig, "\n", "\r\n"), nil, "a\t\nb", "a\r\nb", nil, true},
		{"signature missing", begin + "\nsome text\n", nil, "some text", "some text", sorted(CodeSignatureMissing), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Parse([]byte(tt.in), nil)
			if len(res.Blocks) != 1 || res.Blocks[0].Cleartext == nil {
				t.Fatalf("blocks %+v", res.Blocks)
			}
			b := res.Blocks[0]
			c := b.Cleartext
			if !slices.Equal(c.Hash, tt.hash) || string(c.Text) != tt.text || string(c.Canonical()) != tt.canonical {
				t.Errorf("hash %q text %q canonical %q", c.Hash, c.Text, c.Canonical())
			}
			if got := codes(b.Issues); !slices.Equal(got, tt.codes) {
				t.Errorf("issues %v, want %v", b.Issues, tt.codes)
			}
			if (c.Signature != nil) != tt.signed {
				t.Fatalf("signature %v", c.Signature)
			}
			if tt.signed && (tags(c.Signature.Packets) != "signature" || len(c.Signature.Issues) != 0 && !strings.HasPrefix(tt.name, "quoted")) {
				t.Errorf("signature block %+v", c.Signature)
			}
		})
	}
	// A signed text followed by another block, not a signature.
	res := Parse([]byte(begin+"\ntext\n"+vector(t, "encrypted.asc")), nil)
	if len(res.Blocks) != 2 || !HasIssue(res.Blocks[0].Issues, CodeSignatureMissing) || res.Blocks[1].Type != ArmorMessage {
		t.Errorf("blocks %+v", res.Blocks)
	}
}
