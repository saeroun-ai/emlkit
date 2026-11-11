package legacyenc

import "testing"

// FuzzDecode: no input panics, every block lies inside the body in order, no
// block yields more than its budget nor the body more than its total, and
// Sniff agrees with Scan about the first block.
func FuzzDecode(f *testing.F) {
	for _, fx := range fixtures() {
		f.Add([]byte(fx.text))
		f.Add([]byte(quoted(withCRLF(fx.text), "> ")))
	}
	f.Add([]byte(bombText()))
	f.Add([]byte("begin 644 a\n!80``\n`\nend\n=ybegin size=1 name=b\n=\n=yend crc32=0\n"))
	const (
		maxBody  = 64 << 10
		perBlock = 32 << 10
		total    = 64 << 10
	)
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > maxBody {
			body = body[:maxBody]
		}
		lim := &Limits{MaxBlocks: 8, MaxLineBytes: 1024, MaxBlockBytes: perBlock, MaxTotalBytes: total}
		results, _ := DecodeAll(body, lim)
		prev, sum := 0, 0
		for _, r := range results {
			b := r.Block
			if b.Start < prev || b.End < b.Start || b.End > len(body) {
				t.Fatalf("block %v at %d-%d after %d in %d bytes", b.Kind, b.Start, b.End, prev, len(body))
			}
			prev = b.End
			n := len(r.Data) + len(r.Resource)
			if n > perBlock {
				t.Fatalf("block %v yields %d bytes", b.Kind, n)
			}
			sum += n
		}
		if sum > total {
			t.Fatalf("body yields %d bytes", sum)
		}
		blocks, _ := Scan(body, nil)
		first := KindNone
		if len(blocks) > 0 {
			first = blocks[0].Kind
		}
		if k := Sniff(body); k != first {
			t.Fatalf("Sniff = %v, Scan's first block %v", k, first)
		}
	})
}
