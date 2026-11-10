package mail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/saeroun-ai/emlkit"
)

// Spooling (ROB-110-002 to ROB-110-004, ROB-110-007; issue #43).
//
// A message is read once, in order: an entity's Body is a stream, and once
// read it is gone. A Spool is where a caller that needs the bodies again —
// to scan an attachment after classifying the text, to hand a part to
// another tool — has ReadTree keep them. Small bodies stay in memory, up to a
// quota; the rest go to temporary files, up to another; and a caller with its
// own storage supplies a BlobStore instead of the files.
//
// Every byte a Spool holds is accounted for and released by Close: its
// memory is dropped, its temporary files are removed, and its BlobStore is
// told to delete what it was given. Close may be called more than once, and
// from a defer placed right after NewSpool, whatever happens in between.

// SpoolOptions configures a Spool. The zero value is usable.
type SpoolOptions struct {
	// MemoryBytes is how many bytes of bodies the spool keeps in memory in
	// all. A body that would pass it goes to disk whole, or to the Store.
	// Zero is 8 MiB; a negative value keeps nothing in memory.
	MemoryBytes int64

	// DiskBytes is how many bytes the spool writes to temporary files, or
	// hands the Store, in all. A body that would pass it is cut there, and
	// reported as cut. Zero is 1 GiB; a negative value writes nothing.
	DiskBytes int64

	// Dir is the directory temporary files are created in; empty is the
	// system's.
	Dir string

	// Store, when non-nil, keeps the bodies that do not fit in memory
	// instead of temporary files.
	Store BlobStore
}

// A BlobStore keeps bodies for a Spool somewhere of the caller's choosing.
// The Spool calls it from the goroutine that reads the message.
type BlobStore interface {
	// Create begins a blob and returns its writer and the key it will be
	// read back and deleted by.
	Create() (w io.WriteCloser, key string, err error)
	// Open returns a reader of a blob Create's writer was closed for.
	Open(key string) (io.ReadCloser, error)
	// Delete removes a blob. It is called once for every key Create
	// returned, complete or not.
	Delete(key string) error
}

// A Spool keeps the bodies of one message for repeated reading.
type Spool struct {
	opts SpoolOptions

	mu      sync.Mutex
	closed  bool
	mem     int64
	disk    int64
	entries map[*Node]*spoolEntry
	keys    []string // every blob created, for Close
	diags   []message.Diagnostic
}

type spoolEntry struct {
	data []byte // in memory
	key  string // or in a file, or the store
	size int64
	cut  bool
}

const (
	defaultSpoolMemory = 8 << 20
	defaultSpoolDisk   = 1 << 30
)

// NewSpool returns an empty spool. Close it when the bodies are no longer
// needed.
func NewSpool(opts *SpoolOptions) *Spool {
	s := &Spool{entries: make(map[*Node]*spoolEntry)}
	if opts != nil {
		s.opts = *opts
	}
	if s.opts.MemoryBytes == 0 {
		s.opts.MemoryBytes = defaultSpoolMemory
	}
	if s.opts.DiskBytes == 0 {
		s.opts.DiskBytes = defaultSpoolDisk
	}
	return s
}

// Body is the function to give TreeOptions.Body: it stores each leaf's
// decoded body in the spool. A body the disk quota cuts is kept up to the
// quota and the rest is read and dropped, so the tree's sizes and digests are
// still those of the whole body. A failure of the storage ends ReadTree with
// that error; nothing is left behind that Close does not remove.
func (s *Spool) Body(n *Node, r io.Reader) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("mail: spool is closed")
	}
	memRoom := max(s.opts.MemoryBytes-s.mem, 0)
	diskRoom := max(s.opts.DiskBytes-s.disk, 0)
	s.mu.Unlock()

	e := &spoolEntry{}
	// One byte more than memory has room for says whether the body fits. A
	// body that ends with an error of its own is kept as far as it was read.
	var buf bytes.Buffer
	io.Copy(&buf, io.LimitReader(r, memRoom+1))
	if int64(buf.Len()) <= memRoom {
		e.data = bytes.Clone(buf.Bytes())
		e.size = int64(len(e.data))
		s.mu.Lock()
		s.mem += e.size
		s.entries[n] = e
		s.mu.Unlock()
		return nil
	}

	// Past the memory quota: the whole body goes to a blob.
	if diskRoom <= 0 {
		e.cut = true
		s.note(n, message.CodeLimitDiskSpoolBytes, "mail: the spool's disk quota is spent", "kept nothing of this body; its size and digest are still those of the whole body")
		s.mu.Lock()
		s.entries[n] = e
		s.mu.Unlock()
		return nil
	}
	w, key, err := s.create()
	if err != nil {
		return fmt.Errorf("mail: spool: %w", err)
	}
	e.key = key
	written, werr := copyLimited(w, io.MultiReader(&buf, r), diskRoom)
	cerr := w.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil && !errors.Is(werr, errSpoolQuota) && !isReadError(werr) {
		return fmt.Errorf("mail: spool: %w", werr)
	}
	e.size = written
	if errors.Is(werr, errSpoolQuota) {
		e.cut = true
		s.note(n, message.CodeLimitDiskSpoolBytes, "mail: the spool's disk quota is spent", "kept the first bytes of this body, up to the quota")
	}
	s.mu.Lock()
	s.disk += written
	s.entries[n] = e
	s.mu.Unlock()
	return nil
}

func (s *Spool) note(n *Node, code message.Code, msg, action string) {
	s.mu.Lock()
	s.diags = append(s.diags, message.Diagnostic{
		Code: code, Severity: message.SeverityLimitExceeded, Message: msg, Action: action,
		Path: append([]int(nil), n.Path...),
	})
	s.mu.Unlock()
}

var errSpoolQuota = errors.New("mail: spool quota reached")

// readError marks an error that came from the body being read, not from the
// storage being written.
type readError struct{ err error }

func (e readError) Error() string { return e.err.Error() }
func (e readError) Unwrap() error { return e.err }

func isReadError(err error) bool {
	_, ok := err.(readError)
	return ok
}

// copyLimited copies r to w, at most limit bytes, and reports errSpoolQuota
// when r held more. What is past the limit is read and dropped.
func copyLimited(w io.Writer, r io.Reader, limit int64) (int64, error) {
	var written int64
	buf := make([]byte, 32<<10)
	over := false
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if room := limit - written; int64(n) > room {
				chunk = chunk[:max(room, 0)]
				over = true
			}
			if len(chunk) > 0 {
				m, werr := w.Write(chunk)
				written += int64(m)
				if werr != nil {
					return written, werr
				}
			}
		}
		if rerr != nil {
			switch {
			case over:
				return written, errSpoolQuota
			case rerr == io.EOF:
				return written, nil
			}
			return written, readError{rerr}
		}
	}
}

// create begins a blob, in the store or in a temporary file.
func (s *Spool) create() (io.WriteCloser, string, error) {
	if s.opts.Store != nil {
		w, key, err := s.opts.Store.Create()
		if err == nil {
			s.mu.Lock()
			s.keys = append(s.keys, key)
			s.mu.Unlock()
		}
		return w, key, err
	}
	f, err := os.CreateTemp(s.opts.Dir, "emlkit-spool-*")
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	s.keys = append(s.keys, f.Name())
	s.mu.Unlock()
	return f, f.Name(), nil
}

// Open returns a reader of the decoded body the spool keeps for n, from its
// first byte; it may be called any number of times, and the readers are
// independent. ok is false for a node the spool holds no body of: a
// container, a node of another tree, or any node after Close. The caller
// closes the reader.
func (s *Spool) Open(n *Node) (rc io.ReadCloser, ok bool, err error) {
	s.mu.Lock()
	e := s.entries[n]
	closed := s.closed
	s.mu.Unlock()
	if e == nil || closed {
		return nil, false, nil
	}
	if e.key == "" {
		return io.NopCloser(bytes.NewReader(e.data)), true, nil
	}
	if s.opts.Store != nil {
		rc, err = s.opts.Store.Open(e.key)
	} else {
		rc, err = os.Open(e.key)
	}
	if err != nil {
		return nil, true, err
	}
	return rc, true, nil
}

// Kept reports how many bytes of n's body the spool keeps, and whether the
// disk quota cut it short of the whole body.
func (s *Spool) Kept(n *Node) (size int64, cut bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[n]
	if e == nil || s.closed {
		return 0, false, false
	}
	return e.size, e.cut, true
}

// Usage reports how many bytes the spool holds in memory and outside it.
func (s *Spool) Usage() (memory, disk int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mem, s.disk
}

// Diagnostics returns what the spool's quotas cut: one limit.disk_spool_bytes
// diagnostic for each body cut or not kept, at that body's path.
func (s *Spool) Diagnostics() []message.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]message.Diagnostic(nil), s.diags...)
}

// Close releases everything the spool holds: its memory, and every temporary
// file or stored blob it created, complete or not. It returns the first error
// of a removal and goes on removing the rest. Close may be called again, and
// does nothing then. Readers Open returned before Close keep whatever the
// operating system or the store lets them keep.
func (s *Spool) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	keys := s.keys
	s.keys, s.entries = nil, nil
	s.mem, s.disk = 0, 0
	s.mu.Unlock()

	var first error
	for _, key := range keys {
		var err error
		if s.opts.Store != nil {
			err = s.opts.Store.Delete(key)
		} else if err = os.Remove(key); os.IsNotExist(err) {
			err = nil
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}
