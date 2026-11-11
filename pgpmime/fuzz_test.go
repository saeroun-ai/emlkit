package pgpmime

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParse feeds arbitrary bytes to every entry point. The input is capped
// at 64 KiB and the inflation budget at 1 MiB, so one run is bounded.
func FuzzParse(f *testing.F) {
	files, _ := filepath.Glob("testdata/*")
	for _, name := range files {
		if b, err := os.ReadFile(name); err == nil && len(b) < 64<<10 {
			f.Add(b)
		}
	}
	f.Add([]byte("> -----BEGIN PGP SIGNED MESSAGE-----\n> Hash: SHA1\n>\n> - x\n> -----BEGIN PGP SIGNATURE-----\n> \n> iQ==\n> =AAAA\n"))
	f.Add([]byte("-----BEGIN PGP MESSAGE, PART 1/2-----\r\rqANQR1A=\r----END PGP MESSAGE"))
	f.Add([]byte{0xCB, 0xE0, 'b', 0xFE, 1, 2, 3})
	f.Add([]byte{0xA3, 0x01, 0x00, 0x00, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			data = data[:64<<10]
		}
		lim := &Limits{MaxDecompressedBytes: 1 << 20, MaxKeptBytes: 1 << 20, MaxPackets: 512}
		r := Parse(data, lim)
		for _, b := range r.Blocks {
			if b.Start < 0 || b.End < b.Start || b.End > int64(len(data)) {
				t.Fatalf("block range [%d, %d) of %d", b.Start, b.End, len(data))
			}
			if b.Cleartext != nil {
				b.Cleartext.Canonical()
				CleartextRequest(b.Cleartext)
			}
		}
		r.Summary()
		ps, _ := ParsePackets(data, lim)
		for _, p := range ps {
			if p.Length < 0 || p.Depth > DefaultLimits().MaxDepth {
				t.Fatalf("packet %+v", p)
			}
		}
		Classify(Part{MediaType: "application/octet-stream", Filename: "x.gpg", Head: data})
		CheckStructure(Part{MediaType: "multipart/encrypted", Params: map[string]string{"protocol": "application/pgp-encrypted"}},
			[]Part{{MediaType: "application/pgp-encrypted", Head: data}, {MediaType: "text/plain", Head: data}})
		DetachedRequest(data, data, "pgp-sha256", lim)
		EncryptedRequest(data, lim)
		InlineRequest(data, lim)
		c := Canonicalize(data)
		for i, b := range c {
			if b == '\n' && (i == 0 || c[i-1] != '\r') || b == '\r' && (i+1 == len(c) || c[i+1] != '\n') {
				t.Fatalf("lone line ending at %d", i)
			}
		}
	})
}
