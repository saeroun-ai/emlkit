package message

// Resuming what the caller began (#10, #70).
//
// MultipartReader used to build a new iterator on every call, over whatever
// was left of the body. The previous iterator had read ahead into its buffer
// and had handed out a part the caller may not have finished, and both were
// lost: a Drain after the caller had taken one part and read a byte of it
// framed the rest of the body anew, from a position inside a part. Under
// ProfileStrict that failed with "NextPart: EOF"; under recovery it healed
// the rest into nothing and returned nil, with the parse in progress and the
// remaining parts never seen.
//
// An entity from Parse whose Body is still the one the parse gave it now keeps
// the iterator MultipartReader handed out, and MultipartReader returns that
// iterator again until its parts end (then multipart_stuck.go's iterator at the
// end). The iterator remembers the part it last returned. Drain finishes that
// part — its body, or its own parts, recursively, the same way — before the
// iterator moves on, so a Drain after any partial reading reads the rest of the
// message once, in order, with each part's own path. Walk numbers the parts of
// a resumed iterator from where it stands.
//
// A part the iterator went past before it was read to its end was discarded,
// as NextPart has always documented. Its body can still be read and returns
// what it returns, typically nothing, but it is abandoned: neither it nor
// anything inside it — the parts of a multipart, an embedded message, parts a
// nested iterator still frames from its read-ahead — ever counts as read for
// Completion, so the parse can never be complete. Nothing a Drain reads is
// abandoned, since Drain finishes a part before the iterator moves on.
//
// The state is one iterator per multipart whose parts have not ended and one
// part per iterator: the path from the root to the part being read, bounded by
// Limits.MaxMIMEDepth. The iterator is dropped when its parts end, and a part
// is forgotten when the iterator moves past it.
//
// A body the caller replaced is framed anew on every call, as before, and is
// never kept: the iterator over the original body, and the part it handed
// out, are not reached through it any more, so Drain neither finishes nor
// abandons that part. Entities from Read, ReadWithOptions and New have no
// session, keep no iterator, and are unchanged.

// resumable returns the iterator already handed out over the body the parse
// gave e, or nil.
func (e *Entity) resumable() *multipartReader {
	if e.active == nil || !sameReader(e.Body, e.decodedBody) {
		return nil
	}
	return e.active
}

// keepActive keeps r, just built by MultipartReader, when it iterates the body
// the parse gave e.
func (e *Entity) keepActive(r *multipartReader) {
	if e.parse.session == nil || !sameReader(e.Body, e.decodedBody) {
		return
	}
	r.abandoned = e.abandoned
	e.active = r
}

// consumed reports whether e was read to its end: a leaf body to the read that
// ended it, a multipart to the end of its parts. An entity whose end is not
// tracked — none from Parse — counts as consumed.
func (e *Entity) consumed() bool {
	switch {
	case e.guard != nil:
		return e.guard.ended
	case e.framesParts():
		return e.partsEnded
	}
	return true
}

// abandon marks e, and whatever of it is still being read, as gone past
// before it was read to its end; see above.
func (e *Entity) abandon() {
	if e.abandoned {
		return
	}
	e.abandoned = true
	if e.guard != nil {
		e.guard.abandoned = true
	}
	if r := e.active; r != nil {
		r.abandoned = true
		r.leaveCurrent()
	}
	if emb := e.embedded; emb != nil && !emb.consumed() {
		emb.abandon()
	}
}

// leaveCurrent forgets the part r last returned, abandoning it when it was not
// read to its end. It is called just before r's reader moves on, which
// discards what is left of that part.
func (r *multipartReader) leaveCurrent() {
	c := r.current
	if c == nil {
		return
	}
	r.current = nil
	if !c.consumed() {
		c.abandon()
	}
}

// finishCurrent drains the part the iterator over e's body last returned, when
// there is one and e's body is still the parse's: Drain calls it before the
// iterator moves on.
func (e *Entity) finishCurrent() error {
	r := e.resumable()
	if r == nil || r.current == nil {
		return nil
	}
	return r.current.Drain()
}

// walkStart is the index Walk gives the part before the first one mr will
// return: -1 for a new iterator, and for a resumed one the index of the part
// it last returned.
func walkStart(mr MultipartReader) int {
	if r, ok := mr.(*multipartReader); ok {
		return r.handed - 1
	}
	return -1
}
