package mail_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// The differential harness (ROB-130-004, ROB-130-005). Another parser — a
// driver — dumps each fixture in a neutral JSON schema, emlkit dumps the same
// fixtures in the same schema from message.Parse and ReadTree, and the two are
// compared field by field. A difference is either explained by an entry of
// testdata/differential/exceptions.json, with its rationale, or it fails the
// test. See docs/robustness-notes/differential.md.
//
// The test runs only with EMLKIT_DIFFERENTIAL=1:
//
//	EMLKIT_DIFFERENTIAL_DRIVER  the driver's command line, split at white
//	                            space, to which the fixture paths are
//	                            appended; python3 -I with
//	                            scripts/diff/py_email_dump.py by default
//	EMLKIT_DIFFERENTIAL_DIR     the directories whose *.eml files are
//	                            compared, separated as in PATH;
//	                            testdata/corpus and
//	                            testdata/differential/fixtures by default
//	EMLKIT_DIFFERENTIAL_OUT     a file to write the whole comparison to, as
//	                            JSON; relative to the mail directory

const (
	dfSchema           = "emlkit-differential/1"
	dfReportSchema     = "emlkit-differential-report/1"
	dfExceptionsSchema = "emlkit-differential-exceptions/1"

	// dfMaxInput is the largest fixture compared, the same bound the Python
	// driver applies by default.
	dfMaxInput = 32 << 20
	// dfMaxDriverOutput bounds what is kept of the driver's standard output,
	// and dfMaxDriverStderr of its standard error.
	dfMaxDriverOutput = 64 << 20
	dfMaxDriverStderr = 64 << 10
	// dfDriverDeadline bounds the driver's run over all fixtures.
	dfDriverDeadline = 120 * time.Second
	// dfMaxDepth bounds how deep the emlkit dump and the comparison recurse.
	dfMaxDepth = 128
)

// dfAddress is one address of a From or To field.
type dfAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// dfPart is one entity of the tree. A nil pointer or slice is JSON null: the
// field is absent, or the part is a leaf (Children) or a container (body).
type dfPart struct {
	Path             []int     `json:"path"`
	MediaType        string    `json:"media_type"`
	Charset          *string   `json:"charset"`
	Disposition      *string   `json:"disposition"`
	Filename         *string   `json:"filename"`
	ContentID        *string   `json:"content_id"`
	TransferEncoding *string   `json:"transfer_encoding"`
	BodyLength       *int64    `json:"body_length"`
	BodySHA256       *string   `json:"body_sha256"`
	TextSHA256       *string   `json:"text_sha256"`
	Children         []*dfPart `json:"children"`
	Defects          []string  `json:"defects"`
}

// dfMessage is what a parser made of one fixture.
type dfMessage struct {
	Schema        string      `json:"schema"`
	Parser        string      `json:"parser"`
	ParserVersion string      `json:"parser_version,omitempty"`
	Policy        string      `json:"policy,omitempty"`
	Fixture       string      `json:"fixture"`
	File          string      `json:"file,omitempty"`
	Error         string      `json:"error,omitempty"`
	Subject       *string     `json:"subject"`
	From          []dfAddress `json:"from"`
	To            []dfAddress `json:"to"`
	Date          *string     `json:"date"`
	Root          *dfPart     `json:"root"`
}

// dfDifference is one field on which the two parsers disagree.
type dfDifference struct {
	Parser    string          `json:"parser"`
	Fixture   string          `json:"fixture"`
	Path      string          `json:"path"`
	Field     string          `json:"field"`
	Emlkit    json.RawMessage `json:"emlkit"`
	Other     json.RawMessage `json:"other"`
	Exception *dfException    `json:"exception,omitempty"`
}

// dfException explains the differences it matches. Parser, Fixture, Path and
// Field are each an exact value, "*", or a prefix followed by "*".
type dfException struct {
	Parser    string `json:"parser"`
	Fixture   string `json:"fixture"`
	Path      string `json:"path"`
	Field     string `json:"field"`
	Rationale string `json:"rationale"`
	// Open marks a difference where emlkit may be the one that is wrong. It
	// is listed under "Open differences" in the note, and is kept here only
	// so that the test stays usable until it is resolved.
	Open bool `json:"open,omitempty"`

	matched int
}

type dfExceptionFile struct {
	Schema     string         `json:"schema"`
	Exceptions []*dfException `json:"exceptions"`
}

type dfFixtureResult struct {
	Fixture     string          `json:"fixture"`
	Emlkit      *dfMessage      `json:"emlkit"`
	Other       *dfMessage      `json:"other"`
	Differences []*dfDifference `json:"differences"`
}

type dfReport struct {
	Schema          string             `json:"schema"`
	Parser          string             `json:"parser"`
	Driver          []string           `json:"driver"`
	Summary         dfSummary          `json:"summary"`
	Fixtures        []*dfFixtureResult `json:"fixtures"`
	StaleExceptions []*dfException     `json:"stale_exceptions"`
}

type dfSummary struct {
	Fixtures    int `json:"fixtures"`
	Differences int `json:"differences"`
	Explained   int `json:"explained"`
	Open        int `json:"open"`
	Unexplained int `json:"unexplained"`
	Stale       int `json:"stale_exceptions"`
}

// dfFields are the field names a difference can carry, in the order they are
// compared.
var dfFields = []string{
	"error", "subject", "from", "to", "date",
	"media_type", "charset", "disposition", "filename", "content_id",
	"transfer_encoding", "body_length", "body_sha256", "text_sha256", "children",
}

func TestDifferentialPython(t *testing.T) {
	if os.Getenv("EMLKIT_DIFFERENTIAL") != "1" {
		t.Skip("set EMLKIT_DIFFERENTIAL=1 to compare emlkit with another parser")
	}
	driver := strings.Fields(os.Getenv("EMLKIT_DIFFERENTIAL_DRIVER"))
	if len(driver) == 0 {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not found: " + err.Error())
		}
		driver = []string{"python3", "-I", filepath.Join("..", "scripts", "diff", "py_email_dump.py")}
	}
	dirs := filepath.SplitList(os.Getenv("EMLKIT_DIFFERENTIAL_DIR"))
	defaultDir := len(dirs) == 0
	if defaultDir {
		dirs = []string{
			filepath.Join("..", "testdata", "corpus"),
			filepath.Join("..", "testdata", "differential", "fixtures"),
		}
	}
	var files []string
	names := map[string]string{}
	for _, dir := range dirs {
		found, err := filepath.Glob(filepath.Join(dir, "*.eml"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(found)
		for _, f := range found {
			// Records are matched to files by fixture name, so a name may
			// be used once.
			if prev, ok := names[dfFixtureName(f)]; ok {
				t.Fatalf("fixture %s is both %s and %s", dfFixtureName(f), prev, f)
			}
			names[dfFixtureName(f)] = f
		}
		files = append(files, found...)
	}
	if len(files) == 0 {
		t.Fatalf("no .eml file in %q: nothing to compare", dirs)
	}

	exceptions := dfLoadExceptions(t, filepath.Join("..", "testdata", "differential", "exceptions.json"))

	// The emlkit side is dumped without a CharsetReader, whatever the test
	// binary has installed, so that a text body is the bytes after transfer
	// decoding for every charset, as the schema compares them. Top-level
	// tests run one at a time, and a parallel one only after them all, so no
	// other test sees the change.
	saved := message.CharsetReader
	message.CharsetReader = nil
	t.Cleanup(func() { message.CharsetReader = saved })

	others := dfRunDriver(t, driver, files)
	report := &dfReport{Schema: dfReportSchema, Driver: driver}
	compared := map[string]bool{}
	for _, file := range files {
		fixture := dfFixtureName(file)
		other := others[fixture]
		if other == nil {
			t.Errorf("%s: the driver printed no record for it", fixture)
			continue
		}
		if report.Parser == "" {
			report.Parser = other.Parser
		}
		data, err := dfReadBounded(file)
		if err != nil {
			t.Errorf("%s: %v", fixture, err)
			continue
		}
		mine := dfDumpEmlkit(fixture, data)
		res := &dfFixtureResult{Fixture: fixture, Emlkit: mine, Other: other}
		res.Differences = dfCompare(other.Parser, mine, other)
		if res.Differences == nil {
			res.Differences = []*dfDifference{}
		}
		compared[fixture] = true
		report.Fixtures = append(report.Fixtures, res)
	}

	s := &report.Summary
	s.Fixtures = len(report.Fixtures)
	for _, res := range report.Fixtures {
		for _, d := range res.Differences {
			s.Differences++
			d.Exception = dfExplain(exceptions, d)
			switch {
			case d.Exception == nil:
				s.Unexplained++
				t.Errorf("unexplained difference: %s", dfJSON(d))
			case d.Exception.Open:
				s.Open++
				s.Explained++
				t.Logf("open difference: %s", dfJSON(d))
			default:
				s.Explained++
			}
		}
	}
	// An exception that matched nothing is stale: the difference it explained
	// is gone, and so should it be. One for another parser does not apply to
	// this run, nor, when other fixtures than the default ones are compared,
	// one whose fixture is not among them.
	report.StaleExceptions = []*dfException{}
	notForRun := 0
	for _, x := range exceptions {
		if x.matched > 0 {
			continue
		}
		inRun := false
		for f := range compared {
			if dfMatch(x.Fixture, f) {
				inRun = true
				break
			}
		}
		if !dfMatch(x.Parser, report.Parser) || (!inRun && !defaultDir) {
			notForRun++
			continue
		}
		s.Stale++
		report.StaleExceptions = append(report.StaleExceptions, x)
		t.Errorf("exception matches no difference: %s", dfJSON(x))
	}
	t.Logf("%s: %d fixtures, %d differences, %d explained (%d open), %d unexplained, %d stale exceptions, %d exceptions not for this run",
		report.Parser, s.Fixtures, s.Differences, s.Explained, s.Open, s.Unexplained, s.Stale, notForRun)

	if out := os.Getenv("EMLKIT_DIFFERENTIAL_OUT"); out != "" {
		var raw bytes.Buffer
		enc := json.NewEncoder(&raw)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, raw.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// dfFixtureName is a fixture's name: the file's base name without ".eml", as
// the drivers name it.
func dfFixtureName(file string) string {
	return strings.TrimSuffix(filepath.Base(file), ".eml")
}

func dfReadBounded(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, dfMaxInput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > dfMaxInput {
		return nil, fmt.Errorf("larger than %d bytes", dfMaxInput)
	}
	return data, nil
}

func dfLoadExceptions(t *testing.T, file string) []*dfException {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var xf dfExceptionFile
	if err := dec.Decode(&xf); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if xf.Schema != dfExceptionsSchema {
		t.Fatalf("%s: schema %q, want %q", file, xf.Schema, dfExceptionsSchema)
	}
	for i, x := range xf.Exceptions {
		if x.Parser == "" || x.Fixture == "" || x.Path == "" || x.Field == "" || strings.TrimSpace(x.Rationale) == "" {
			t.Fatalf("%s: exception %d needs a parser, fixture, path, field and rationale: %s", file, i, dfJSON(x))
		}
		known := false
		for _, f := range dfFields {
			if dfMatch(x.Field, f) {
				known = true
			}
		}
		if !known {
			t.Fatalf("%s: exception %d names no known field: %s", file, i, dfJSON(x))
		}
	}
	return xf.Exceptions
}

// dfMatch reports whether s matches pattern: "*", a prefix followed by "*",
// or the exact value.
func dfMatch(pattern, s string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(s, prefix)
	}
	return pattern == s
}

// dfExplain returns the first exception that matches d, or nil.
func dfExplain(exceptions []*dfException, d *dfDifference) *dfException {
	for _, x := range exceptions {
		if dfMatch(x.Parser, d.Parser) && dfMatch(x.Fixture, d.Fixture) && dfMatch(x.Path, d.Path) && dfMatch(x.Field, d.Field) {
			x.matched++
			return x
		}
	}
	return nil
}

// dfMarshal is json.Marshal without the escaping of <, > and &, which
// addresses and markup are full of.
func dfMarshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(strconv.Quote(fmt.Sprint(v)))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func dfJSON(v any) string {
	return string(dfMarshal(v))
}

// dfCappedBuffer keeps at most max bytes and discards the rest, so that a
// driver that writes too much still runs to its end and the excess is
// reported instead of held.
type dfCappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *dfCappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.over = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}

// dfRunDriver runs the driver once over all files, under a deadline, and
// returns its records by fixture.
func dfRunDriver(t *testing.T, driver, files []string) map[string]*dfMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dfDriverDeadline)
	defer cancel()
	args := append(append([]string(nil), driver[1:]...), files...)
	cmd := exec.CommandContext(ctx, driver[0], args...)
	cmd.WaitDelay = 5 * time.Second
	stdout := &dfCappedBuffer{max: dfMaxDriverOutput}
	stderr := &dfCappedBuffer{max: dfMaxDriverStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("driver %q: %v\n%s", driver, err, stderr.buf.Bytes())
	}
	if stdout.over {
		t.Fatalf("driver %q wrote more than %d bytes", driver, dfMaxDriverOutput)
	}
	out := map[string]*dfMessage{}
	dec := json.NewDecoder(&stdout.buf)
	for {
		var m dfMessage
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("driver %q: record %d: %v", driver, len(out), err)
		}
		if m.Schema != dfSchema {
			t.Fatalf("driver %q: %s: schema %q, want %q", driver, m.Fixture, m.Schema, dfSchema)
		}
		if m.Parser == "" || m.Fixture == "" {
			t.Fatalf("driver %q: record %d names no parser or fixture", driver, len(out))
		}
		if out[m.Fixture] != nil {
			t.Fatalf("driver %q: two records for %s", driver, m.Fixture)
		}
		out[m.Fixture] = &m
	}
	return out
}

// dfDumpEmlkit is the emlkit driver: what message.Parse, with the default
// options, and ReadTree, descending into embedded messages, make of data.
func dfDumpEmlkit(fixture string, data []byte) *dfMessage {
	m := &dfMessage{Schema: dfSchema, Parser: "emlkit", Fixture: fixture}
	session, e, err := message.Parse(bytes.NewReader(data), nil)
	if e == nil {
		m.Error = fmt.Sprintf("parse: %v", err)
		return m
	}
	tree, err := mail.ReadTree(e, &mail.TreeOptions{
		DescendMessages: true,
		MaxBodyBytes:    dfMaxInput,
		MaxTotalBytes:   -1,
	})
	if err != nil {
		m.Error = fmt.Sprintf("tree: %v", err)
	}
	if tree == nil || tree.Root == nil {
		if m.Error == "" {
			m.Error = "tree: no root"
		}
		return m
	}

	nodes := map[string]*dfPart{}
	m.Root = dfPartOf(tree.Root, nodes, 0)
	diags := append(append([]message.Diagnostic(nil), session.Diagnostics()...), tree.Diagnostics...)
	for _, d := range diags {
		// A diagnostic belongs to the part at its path, or, when no part is
		// there, to the nearest one above it, with its path.
		code := string(d.Code)
		for n := len(d.Path); n >= 0; n-- {
			if p := nodes[fmt.Sprint(d.Path[:n])]; p != nil {
				if n < len(d.Path) {
					code += " " + fmt.Sprint(d.Path)
				}
				p.Defects = append(p.Defects, code)
				break
			}
		}
	}

	h := mail.Header{Header: tree.Root.Header}
	if h.Has("Subject") {
		s, _ := h.Subject()
		m.Subject = &s
	}
	m.From = dfAddresses(&h, "From")
	m.To = dfAddresses(&h, "To")
	if h.Has("Date") {
		if r := h.DateTolerant(); r.Valid {
			s := r.Time.UTC().Format("2006-01-02T15:04:05Z")
			m.Date = &s
		}
	}
	return m
}

func dfAddresses(h *mail.Header, key string) []dfAddress {
	if !h.Has(key) {
		return nil
	}
	out := []dfAddress{}
	for _, a := range h.AddressListTolerant(key, nil).Addresses {
		out = append(out, dfAddress{Name: a.Name, Address: a.Address})
	}
	return out
}

func dfOptional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func dfPartOf(n *mail.Node, nodes map[string]*dfPart, depth int) *dfPart {
	p := &dfPart{
		Path:             append([]int{}, n.Path...),
		MediaType:        n.MediaType,
		Charset:          dfOptional(strings.ToLower(strings.TrimSpace(n.Params["charset"]))),
		Disposition:      dfOptional(n.Disposition),
		Filename:         dfOptional(n.Filename),
		ContentID:        dfOptional(n.ContentID),
		TransferEncoding: dfOptional(strings.ToLower(strings.TrimSpace(n.Header.Get("Content-Transfer-Encoding")))),
		Defects:          []string{},
	}
	nodes[fmt.Sprint(n.Path)] = p
	if len(n.Children) > 0 || n.Role == mail.RoleContainer {
		p.Children = []*dfPart{}
		if depth < dfMaxDepth {
			for _, c := range n.Children {
				p.Children = append(p.Children, dfPartOf(c, nodes, depth+1))
			}
		}
		return p
	}
	// A leaf whose body was read has a digest; one the parse stopped before
	// has none, and its body is unknown.
	if n.SHA256 == ([32]byte{}) {
		return p
	}
	size, sum := n.Size, hex.EncodeToString(n.SHA256[:])
	p.BodyLength, p.BodySHA256 = &size, &sum
	// Without a CharsetReader the text of a part is its bytes only when its
	// charset needs no conversion; for any other charset emlkit's text is not
	// known here, and text_sha256 stays null.
	if strings.HasPrefix(n.MediaType, "text/") && !n.Truncated {
		switch p.charset() {
		case "", "utf-8", "us-ascii":
			p.TextSHA256 = &sum
		}
	}
	return p
}

func (p *dfPart) charset() string {
	if p.Charset == nil {
		return ""
	}
	return *p.Charset
}

// dfCompare compares what two parsers made of one fixture.
func dfCompare(parser string, mine, other *dfMessage) []*dfDifference {
	var out []*dfDifference
	field := func(path []int, name string, a, b any) {
		ja, jb := dfMarshal(a), dfMarshal(b)
		if !bytes.Equal(ja, jb) {
			out = append(out, &dfDifference{
				Parser: parser, Fixture: mine.Fixture, Path: fmt.Sprint(path), Field: name,
				Emlkit: ja, Other: jb,
			})
		}
	}
	if mine.Error != "" || other.Error != "" || mine.Root == nil || other.Root == nil {
		field(nil, "error", dfOptional(mine.Error), dfOptional(other.Error))
		if mine.Root == nil || other.Root == nil {
			return out
		}
	}
	field(nil, "subject", mine.Subject, other.Subject)
	field(nil, "from", mine.From, other.From)
	field(nil, "to", mine.To, other.To)
	field(nil, "date", mine.Date, other.Date)

	var part func(a, b *dfPart, depth int)
	part = func(a, b *dfPart, depth int) {
		path := a.Path
		field(path, "media_type", a.MediaType, b.MediaType)
		field(path, "charset", a.Charset, b.Charset)
		field(path, "disposition", a.Disposition, b.Disposition)
		field(path, "filename", a.Filename, b.Filename)
		field(path, "content_id", a.ContentID, b.ContentID)
		field(path, "transfer_encoding", a.TransferEncoding, b.TransferEncoding)
		count := func(c []*dfPart) *int {
			if c == nil {
				return nil
			}
			n := len(c)
			return &n
		}
		field(path, "children", count(a.Children), count(b.Children))
		// A part one parser framed into parts and the other kept whole
		// differs in its children alone: its body fields follow from that.
		if (a.Children == nil) == (b.Children == nil) {
			field(path, "body_length", a.BodyLength, b.BodyLength)
			field(path, "body_sha256", a.BodySHA256, b.BodySHA256)
			// The text is compared where both parsers decoded it from the
			// same bytes, so that it isolates charset handling.
			if a.TextSHA256 != nil && b.TextSHA256 != nil && a.BodySHA256 != nil && b.BodySHA256 != nil && *a.BodySHA256 == *b.BodySHA256 {
				field(path, "text_sha256", a.TextSHA256, b.TextSHA256)
			}
		}
		if depth >= dfMaxDepth {
			return
		}
		for i := 0; i < len(a.Children) && i < len(b.Children); i++ {
			part(a.Children[i], b.Children[i], depth+1)
		}
	}
	part(mine.Root, other.Root, 0)
	return out
}
