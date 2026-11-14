package message

import (
	"io"

	"github.com/saeroun-ai/emlkit/internal/errgraph"
)

// An iterator that can frame nothing more (ROB-130-007).
//
// textproto's MultipartReader does not end its iteration on an ordinary
// failure, such as a source that failed and may recover, or a boundary it
// cannot use. Each NextPart then tries again and returns the failure anew,
// as a new value each time. Two of those failures are final. While the
// iterator kept returning a fresh value for them, it could not say so by
// repeating itself, which is how mail.ReadTree (and any caller that goes on
// past a refused part) learns that an iteration has ended for good. The soak
// found ReadTree spinning forever on both:
//
//   - a multipart that declares no boundary where nothing heals it
//     (ProfileStrict, Read, ReadWithOptions): textproto refuses the empty
//     boundary on every call, before it reads anything;
//   - under Parse, an input that failed while a multipart was framed: the
//     parse's input returns its first error from then on (progress.go), so no
//     later call can read past it;
//   - a source that ended with a FatalError of its own, such as the body of a
//     message/rfc822 carrier past Limits.MaxDecodedBodyBytes, framed as the
//     embedded message's multipart: that body returns the same failure on
//     every later read. Every stop of the session is returned before this
//     point, so a FatalError still inside err is its source's.
//
// The iterator keeps such a failure and returns that identical value on every
// later call, and the multipart counts as read to its end for Completion, as
// for any iterator that fails for good. Every other failure, one the next call
// may get past (a framing line too long to read, a part header that could not
// be read), is returned as before.

// skEndsForGood reports whether err, a failure of r's reader other than
// io.EOF, is one that no later call can get past.
func (r *multipartReader) skEndsForGood(err error) bool {
	if !r.healing && r.declared == "" || IsFatal(err) {
		return true
	}
	failed := r.parent.session.skInputFailure()
	return failed != nil && errgraph.Find(err, func(node error) bool { return sameError(node, failed) })
}

// A multipart already read to its end (ROB-130-007).
//
// MultipartReader builds a new iterator on every call, over whatever is left
// of the body. Once an iterator has read the parts to their end, what is left
// is the epilogue, and under recovery a new iterator healed it into parts of
// its own: a multipart.empty diagnostic, or a part with no header holding the
// epilogue, recorded in the session and counted by Completion. Drain called
// again, as it says it may be, did exactly that through Walk. How much
// epilogue was left depended on how far the first iterator had read ahead, so
// the verdict also depended on how the input was chunked. An entity from
// Parse whose parts were read to their end is now handed an iterator at its
// end. A body the caller replaced keeps the earlier behavior.
//
// ProfileStrict did the same without healing (#70): its new iterator searched
// the epilogue for the boundary, so a second Drain returned the iterator's
// "NextPart: EOF" after a first that returned nil, and an epilogue holding a
// delimiter line made up parts. It reads none of the epilogue now, under every
// profile. The iterator at the end returns the failure the parts ended with,
// if they ended with one, so a repeated Walk or Drain reports what the first
// did rather than nil. Entities from Read, ReadWithOptions and New have no
// session, never mark their parts ended, and are unchanged.

// skPartsRead reports whether e's parts were read to their end, from the body
// the parse gave it.
func (e *Entity) skPartsRead() bool {
	return e.partsEnded && sameReader(e.Body, e.decodedBody)
}

// skEndedParts is an iterator at its end. Like every scope of the parse, it
// reports the parse's stop, once there is one, rather than its end; otherwise
// the failure the parts ended with, or io.EOF.
type skEndedParts struct{ e *Entity }

func (r skEndedParts) NextPart() (*Entity, error) {
	if stop := r.e.parse.fatalErr(); stop != nil {
		return nil, stop
	}
	if err := r.e.partsErr; err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (skEndedParts) Close() error { return nil }

// skInputFailure returns the error the input of the parse failed with: its
// first error, when that was not io.EOF, which every later read of the input
// returns. It is nil while the input has not failed, and for a nil session.
func (s *ParseSession) skInputFailure() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputEnd == nil || s.inputEnd == io.EOF {
		return nil
	}
	return s.inputEnd
}
