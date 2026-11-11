package textproto

import "fmt"

// A framing read that fails where the last one failed (ROB-130-007).
//
// An ordinary read failure does not end the iteration: a source may recover,
// and the next NextPart reads it again. Each failure used to be returned
// wrapped anew, though, so a source that never recovered made NextPart return
// a new value on every call, and a caller that goes on past a failed part
// until the iterator repeats itself (mail.ReadTree) never stopped. The soak
// found that with a base64 carrier whose decoder failed under the embedded
// message's multipart. A failure that recurs where the last one began, with
// nothing read since, and is the identical error, is now returned as the
// identical value the last call returned. A source that recovers is read on as
// before, and a failure anywhere else is wrapped anew as before.

// skFramingFailure is the last failure of a framing read: where the read
// began, what the source failed with, and what NextPart returned for it.
type skFramingFailure struct {
	at       int64
	cause    error
	returned error
}

// wrap returns what NextPart returns for err, a failure of a framing read
// that began at start.
func (f *skFramingFailure) wrap(err error, start int64) error {
	if f.returned != nil && f.at == start && skSameError(f.cause, err) {
		return f.returned
	}
	f.at, f.cause = start, err
	f.returned = fmt.Errorf("multipart: NextPart: %w", err)
	return f.returned
}

// skSameError reports whether a and b are the same error value, calling no
// method of either. Two values of one dynamic type that is not comparable are
// different.
func skSameError(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}
