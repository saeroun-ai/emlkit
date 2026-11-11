# AppleSingle and AppleDouble (ROB-140-002)

Code: `appledouble/`. Tests: `appledouble/*_test.go`, `FuzzParse`.

`github.com/saeroun-ai/emlkit/appledouble` is an optional package. The core
does not import it and it imports nothing of the core; it has its own `Issue`
type. It reads the AppleSingle (magic `0x00051600`) and AppleDouble
(`0x00051607`) formats of RFC 1740 and Apple's specification, versions 1
(`0x00010000`) and 2 (`0x00020000`), from the decoded bytes of a MIME part.

```go
f, err := appledouble.Parse(body, nil) // err only when body is neither format
desc, err := appledouble.Combine(headerPart, dataPart, nil)
```

## Entries

Every descriptor read is in `File.Entries` with its declared offset and
length and the bytes the input holds (`Data`, `Truncated`). The first entry of
each kind below also has a typed view; a repeated id is reported and the
first is used.

| Id | Entry | View |
| --- | --- | --- |
| 1 | data fork | `File.DataFork` |
| 2 | resource fork | `File.Resource`: the bytes, and the map's resources listed (type, id, name, attributes, data slice); nothing is interpreted |
| 3 | real name | `File.Name`: UTF-8 when the bytes are valid UTF-8, otherwise MacRoman (`charmap.Macintosh`), composed to NFC, trailing NULs dropped; `Raw` kept |
| 4 | comment | `File.Comment`, raw |
| 8 | file dates | `File.Dates`: created, modified, backup, accessed; signed seconds from 2000-01-01 UTC, `0x80000000` is unknown (zero `time.Time`) |
| 9 | Finder info | `File.Finder`: type and creator codes, Finder flags, extended flags, the 32 raw bytes; and the extended attributes macOS stores after them |
| 13 | short name | `File.ShortName`, decoded as the real name |
| 5, 6, 7, 10, 11, 12, 14, 15, others | icons, v1 file info, Macintosh/ProDOS/MS-DOS/AFP file info, directory id, unknown ids | raw in `Entries`; `EntryID.String` names the known ones |

Extended attributes: when the Finder info entry has the `ATTR` header at its
byte 34 (after 32 bytes of Finder info and 2 of padding, as xnu writes `._`
files), each attribute's name and value is listed — `com.apple.quarantine`,
`com.apple.metadata:kMDItemWhereFroms` and so on. Value offsets are offsets
into the whole input, as macOS writes them. Values are not interpreted.

## Budgets

`Limits`, zero value and nil are the defaults:

| Limit | Default | When reached |
| --- | --- | --- |
| `MaxEntries` | 64 | further descriptors are not read; `budget.entries` |
| `MaxEntryBytes` | 16 MiB | an entry or attribute value is cut; `budget.entry_bytes` |
| `MaxTotalBytes` | 64 MiB | entry bytes in all; later entries are cut or empty; `budget.total_bytes` |
| `MaxAttributes` | 128 (negative: none listed) | `budget.attributes` |
| `MaxResources` | 1024 (negative: map not walked) | `budget.resources`; bounds the map walk |
| `MaxIssues` | 64 | one `budget.issues`, the rest counted in `DroppedIssues` |

Names are decoded from at most 1024 bytes (`name.long` beyond). Nothing is
copied: every byte slice aliases the input, so the budgets bound what is
handed over, and every list Parse builds is bounded by one of them.

## Tolerance

- Only an input that does not begin with either magic number (or is shorter
  than four bytes) is an error, `ErrNotAppleFile`. Everything else yields a
  `File` with what could be read and an `Issue` per defect, each with a
  stable `Code`, the entry concerned and an input offset.
- A header cut short (`header.truncated`) keeps what precedes the cut: the
  version, the filler, the descriptors that fit. An unknown version is read
  as version 2 (`header.version`).
- An entry that starts or ends past the input (`entry.out_of_range`) gets the
  bytes the input holds. Offsets and lengths are 32-bit values added in
  64-bit arithmetic and checked against the input before slicing.
- Entries that overlap one another or the descriptor table
  (`entry.overlap`) each keep their declared bytes. A data fork inside an
  AppleDouble header is kept and reported (`entry.unexpected`).
- A file dates or Finder info entry shorter than its layout is read as far as
  it goes; the dates missing are unknown. Short file dates, and Finder info
  without a whole type and creator, are `entry.short`. A Finder info of 8 to
  31 bytes is not reported.
- The attribute header and table, and the resource map, its type list,
  references, names and data, are walked only within the entry; a part that
  runs out is reported (`xattr.*`, `rsrc.*`) and what lies before is listed.
- `Combine` reports `combine.missing_data` (no data part and no data fork
  entry) and `combine.data_conflict` (both; the data part is used). A header
  that is neither format returns `ErrNotAppleFile` with the data part still
  in the `Description`.

## MIME helpers

`Recognize` maps a media type (parameters, space and case ignored, ASCII
folding only) to `KindAppleFile` (`application/applefile`), `KindAppleDouble`
(`multipart/appledouble`) or `KindBinHex` (`application/mac-binhex40` and
`application/x-mac-binhex40`, recognised only). `AppleDoubleParts` picks the
header part and the data part from the parts' media types: the first
`application/applefile`, and the first other part after it (before it when
there is none after). `Combine` takes the two parts' decoded bytes and
returns the name, type and creator, flags, dates, data, resource fork and
attributes, with the parsed `File`.

Wiring to `mail.ReadTree` (the package's own documentation has the same
code; `TestReadTreeWiring` runs it on a message laid out as Apple Mail sends
one):

```go
tree, _ := mail.ReadTree(root, nil)
tree.Root.Walk(func(n *mail.Node) bool {
	if appledouble.Recognize(n.MediaType) != appledouble.KindAppleDouble {
		return true
	}
	types := make([]string, len(n.Children))
	for i, c := range n.Children {
		types[i] = c.MediaType
	}
	h, d := appledouble.AppleDoubleParts(types)
	if h < 0 {
		return true
	}
	var data []byte
	if d >= 0 {
		data = n.Children[d].Data
	}
	desc, err := appledouble.Combine(n.Children[h].Data, data, nil)
	// desc.Name, desc.Type, desc.Creator, desc.Data, desc.Issues ...
	return true
})
```

An `application/applefile` node outside a `multipart/appledouble` is an
AppleSingle file: `Combine(n.Data, nil, nil)`. `Node.Data` is cut at
`TreeOptions.MaxBodyBytes`; when `Node.Truncated` is set, the header or data
given to `Combine` is a prefix and the issues say what it lacks. ReadTree
classifies the two parts as it does any part (both are usually
`RoleAttachment`); this package does not change that.

## Limits of the support

- No real Apple Mail sample is available here. The fixtures are built in the
  tests by a small writer from the published layouts (RFC 1740, Apple's
  specification, the `._` layout of xnu's `vfs_xattr.c`), and a synthetic
  Apple Mail-shaped message. They should be checked against real Apple Mail
  messages when some are at hand.
- Reading only: there is no writer, no BinHex 4.0 or MacBinary decoding, and
  no automatic hook in `mail.ReadTree`.
- Version 1's file info entry (7), the Macintosh, ProDOS, MS-DOS and AFP file
  info entries and the directory id are kept raw. Icons, resource contents
  and attribute values (binary plists, quarantine strings) are not
  interpreted.
- The `ATTR` header is looked for only at byte 34 of the Finder info entry;
  attribute values are taken by their own offsets, and a value outside the
  Finder info entry is read and reported (`xattr.outside_entry`). The header's
  total size and data area fields are not cross-checked.
- Names are what the sender says, not safe paths: pass them through
  `message.SafeFilename` before using one on disk.
