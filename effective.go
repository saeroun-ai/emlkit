package message

import (
	"maps"
	"slices"
	"strings"
	"sync"
)

// The effective MIME metadata of an entity (ROB-100-007).
//
// Two answers can be given to "what is this entity's Content-Type", and they
// differ on purpose. Header.ContentType and Header.ContentDisposition parse
// whatever the header holds at the moment they are called, under no budget,
// because a Header belongs to whoever holds it and may never have come from a
// parse. The parse engine instead reads those two fields under the parameter
// budgets of the parse the entity belongs to, and it reads them when the entity
// is constructed: what it read is what chose the transfer and charset decoders,
// whether a text attachment's charset was converted, and whether
// MultipartReader iterates anything and with which boundary.
//
// A layer that classifies an entity — mail.Reader deciding between an inline
// part and an attachment — has to classify it by that second answer. Reparsing
// the raw header would undo the budgets, which bound the work of reading an
// oversized parameter list, and it could disagree with decisions already built
// into the body: a text attachment whose charset was left unconverted because
// the bounded disposition said "attachment" would otherwise be offered as
// inline text. EffectiveContentType and EffectiveContentDisposition are that
// second answer.
//
// # A construction snapshot, not a view of the current header
//
// Entity.Header is exported and can be edited after construction, but an edit
// cannot reach back into a body whose decoders were already chosen from the
// header as it was, nor into a multipart iterator framed by the boundary it
// declared. So the effective metadata is not re-evaluated against the edited
// header: it is what the entity was constructed with, and it stays so for the
// entity's lifetime. An edit is visible through Header.ContentType and
// Header.ContentDisposition, and is written by WriteTo, but it does not
// reinterpret this entity; constructing a new one with New is how an edited
// header is given effect.
//
// The two fields differ only in when they are interpreted. Content-Type is
// interpreted during construction, because the decoders depend on it.
// Content-Disposition is captured during construction and interpreted at most
// once, by whichever of the charset stage and EffectiveContentDisposition asks
// first; a parse never asked about the field reads nothing and reports nothing,
// which keeps the engine's existing rule that a budget applies where the engine
// reads a field. Either way the value interpreted is the field as it was when
// the entity was constructed.
//
// # Diagnostics are recorded once
//
// A field past a parameter budget records one diagnostic when it is
// interpreted, and no later call interprets it again, so asking any number of
// times — from the charset stage, from mail.Reader, and from the caller —
// records nothing more. A reading made after a DiagnosticSink stopped the parse
// still keeps the bounded result, because the budget is what bounds the work,
// but records nothing; ParseStop reports the stop.
//
// # Reading from a DiagnosticSink
//
// A sink is called synchronously, and the first reading of a disposition is one
// of the places that calls it. While it runs, the sink may read the metadata of
// any entity whose metadata has already been interpreted — including the one it
// is being told about — through EffectiveContentType and
// EffectiveContentDisposition, and may call ParseStop. None of them waits on
// anything the delivery in progress holds: the interpretation is published
// before the sink is called, and no lock of the entity is held while it is
// called. Whatever the sink returns afterwards, a stop included, takes effect as
// for any other diagnostic.
//
// What a sink cannot do is make the parse record from inside the delivery. The
// first reading of a disposition that is past a budget records a diagnostic, so
// asking for that from a sink — about an entity nothing has asked about yet — is
// parsing from inside a delivery, which the session does not support for any
// recording, and blocks. Reading a field that is within its budgets records
// nothing and is safe whenever it is first done.
//
// # Results are copies
//
// Every parameter map returned is a fresh copy, so a caller may modify it
// without disturbing the entity, the decoders built from it, or a later call.
// A nil map — no parameters, or a field that did not parse — is returned as
// nil.

// A dispositionSnapshot is the Content-Disposition value an entity was
// constructed with, and the one interpretation of it under the budgets of the
// parse the entity belongs to.
//
// It is referred to by pointer so that copying an Entity value — which callers
// may do — neither copies its lock nor gives the copy a second interpretation
// with a second diagnostic.
//
// # Publication before delivery
//
// Interpreting the field and reporting what the interpretation found are two
// steps, done in that order and never under the same lock. The interpretation
// does not deliver diagnostics: it scans and parses the retained value under
// mu, then publishes the result and sets done before releasing mu. Decoding an
// encoded-word parameter can call the configured CharsetReader; that hook must
// not recursively request the field whose interpretation is still in progress. Only then is the budget diagnostic, if one is
// owed, handed to the session, and so to the caller's DiagnosticSink.
//
// That order is what lets a sink read this entity's metadata. A sink is told
// about the refusal synchronously, from inside the first get, and it may well
// ask the entity what was kept: holding mu across that delivery — or a
// sync.Once, which is a lock by another name — would make that read wait for
// the delivery it is part of, for ever. With publication first, the read finds
// done and returns the published value, and the delivery has nothing of its own
// to wait for.
//
// The diagnostic is still recorded exactly once: only the call that set done
// owes it, and only that call records it. What a concurrent first reader in
// another goroutine may see is the published value before that recording has
// reached the session — the interpretation is final, but ParseStop reports a
// stop the sink asks for in that delivery only once the delivery has finished.
// The session already orders concurrent recordings by whichever reaches it
// first, so this adds no ordering it did not already lack.
type dispositionSnapshot struct {
	ctx parseContext
	raw string

	// rawType is the Content-Type the entity was constructed with. It is not
	// interpreted here — the entity interprets it at construction — but it is
	// what the candidates of its parameters are read from when a caller asks
	// for them, and where the charset to try for raw 8-bit bytes in a
	// disposition parameter comes from.
	rawType string

	mu     sync.Mutex
	done   bool
	disp   string
	params map[string]string
	err    error

	// The candidates of the two fields' parameters, and the filename
	// candidates made from them, each read at most once and only when asked
	// for; see candidates and filenames. cmu guards them and is never held
	// while a diagnostic is delivered.
	cmu        sync.Mutex
	candDone   bool
	typeCands  []ParameterCandidate
	dispCands  []ParameterCandidate
	namesDone  bool
	names      []FilenameCandidate
	namesNoted bool
}

// newDispositionSnapshot captures the Content-Disposition of header, to be
// interpreted later under ctx. Capturing it costs nothing beyond keeping the
// string: no byte of it is read until it is interpreted.
func newDispositionSnapshot(ctx parseContext, header Header) *dispositionSnapshot {
	return &dispositionSnapshot{
		ctx:     ctx,
		raw:     header.Get("Content-Disposition"),
		rawType: header.Get("Content-Type"),
	}
}

// typeCharset returns the charset the captured Content-Type declares, read
// tolerantly and recording nothing, or "" when it declares none. It is the
// charset raw 8-bit bytes in a disposition parameter are tried in.
func (d *dispositionSnapshot) typeCharset() string {
	for _, c := range d.ctx.candidatesOf("Content-Type", d.rawType, nil) {
		if c.Name == "charset" && c.Chosen {
			return c.Value
		}
	}
	return ""
}

// candidates returns the candidates of the parameters of both captured
// fields, reading them on the first call. The slices are the snapshot's own
// and are never written after that; callers outside this file get copies.
func (d *dispositionSnapshot) candidates() (typeCands, dispCands []ParameterCandidate) {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	if !d.candDone {
		d.typeCands = d.ctx.candidatesOf("Content-Type", d.rawType, nil)
		d.dispCands = d.ctx.candidatesOf("Content-Disposition", d.raw, func() string {
			for _, c := range d.typeCands {
				if c.Name == "charset" && c.Chosen {
					return c.Value
				}
			}
			return ""
		})
		d.candDone = true
	}
	return d.typeCands, d.dispCands
}

// get returns the interpretation of the captured value, making it on the first
// call and recording its diagnostic after publishing it; see the type's comment
// for why in that order. The returned map is the snapshot's own and is never
// written after publication; callers outside this file get a copy.
func (d *dispositionSnapshot) get() (string, map[string]string, error) {
	d.mu.Lock()
	if d.done {
		disp, params, err := d.disp, d.params, d.err
		d.mu.Unlock()
		return disp, params, err
	}
	r := d.ctx.interpretDisposition(d.raw, d.typeCharset)
	if r.cancelled {
		// The parse's context was cancelled while the field was being read, so
		// there is no interpretation to publish: nothing is cached, nothing is
		// recorded, and the caller is given the parse's stop. It is built after
		// the lock is released, since building it calls the context's methods. A
		// later call reads the field again and, the context staying done, is
		// cancelled again at once. See cancel.go.
		d.mu.Unlock()
		return "", nil, d.ctx.session.cancelErr()
	}
	d.disp, d.params, d.err, d.done = r.value, r.params, r.err, true
	d.mu.Unlock()

	// No lock of this entity is held from here on, so a sink that reads the
	// entity back while it is told about the refusal gets the value just
	// published.
	d.ctx.recordParameterDiagnostics(r.owed)
	return r.value, r.params, r.err
}

// observedErr returns the error of the interpretation if one has been made,
// and nil if the disposition has not been interpreted yet. Unlike get it never
// interprets anything, so a caller that only needs to know about a failure
// already observed — Entity.Walk — does not cause a reading, or the diagnostic
// a reading may record, that nothing asked for.
func (d *dispositionSnapshot) observedErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.done {
		return nil
	}
	return d.err
}

// localFailure returns the entity-scoped FatalError this entity's own metadata
// was refused with under ProfileStrict — its Content-Type, or a
// Content-Disposition that has already been interpreted — or nil. It is a
// failure of this entity alone and never a stop of the parse; see
// metadata_repair.go. A struct-literal Entity has none.
func (e *Entity) localFailure() error {
	if IsFatal(e.mediaErr) {
		return e.mediaErr
	}
	if e.disposition != nil {
		if err := e.disposition.observedErr(); IsFatal(err) {
			return err
		}
	}
	return nil
}

// isAttachment reports whether the captured disposition is "attachment". It is
// what ReadOptions.SkipTextAttachmentDecoding decides by, and it is the same
// interpretation EffectiveContentDisposition reports, so that the charset
// decision and a later classification cannot disagree.
func (d *dispositionSnapshot) isAttachment() bool {
	disp, _, err := d.get()
	return err == nil && strings.EqualFold(disp, "attachment")
}

// EffectiveContentType returns the media type and parameters this entity was
// constructed with, as the parse it came from interpreted them, and the error
// that interpretation returned. These are the values the entity's transfer and
// charset decoders and its MultipartReader were built from.
//
// For an entity from Parse they are read under Limits.MaxParameterCount and
// Limits.MaxParameterSegments: a Content-Type past either keeps its media type
// and the parameters within the budget, which is what is returned here with a
// nil error, and the one diagnostic for it was recorded when the entity was
// constructed. For an entity from New, Read or ReadWithOptions, which have no
// budgets, they are what Header.ContentType returned for the header as it was
// at construction. A missing Content-Type is "text/plain" with no parameters,
// as there. For those entities a field that did not parse is returned as
// Header.ContentType returns one: the raw value as the media type, no
// parameters, and the error.
//
// Under Parse a field the whole-field parser refuses, or does not read in
// full, is read all the same (ROB-050-001 to ROB-050-008; see
// metadata_repair.go and mediatype_tolerant.go). Under ProfileTolerant and
// ProfileForensic it is repaired: a media type missing its type, subtype or
// slash is read by conservative defaults ("text" is text/plain), values that
// are not tokens and quotes that are never closed are recovered, RFC 2231
// values are assembled in any order and decoded in any charset CharsetReader
// knows, and a repeated parameter takes one value by a fixed precedence — all
// with a nil error, because that is the interpretation the entity was built
// from, and with the repair reported by the diagnostics recorded at
// construction, not by this error. Header.ContentType still returns the legacy
// failure for the same raw field. A parameter value is UTF-8 unless its bytes
// could not be decoded, in which case it holds the bytes and a
// mime.parameter_unknown_charset diagnostic says so. ParameterCandidates returns
// every occurrence of a repeated parameter, where this returns the chosen one.
// Under ProfileStrict a field that needs repair is not interpreted: the media
// type is empty, the parameters nil, and the error the entity-scoped FatalError
// that construction returned, which does not stop the parse. Because the
// Content-Type is read before any decoder is chosen, such an entity's body is
// exactly the bytes of the input. An entity
// ProfileStrict refused for its header's line endings — a line ended by a bare
// '\r', or a last line with no line ending (ROB-010-002) — is reported the same
// way, whatever its Content-Type says: nothing of the field is interpreted. So
// is one refused for a byte order mark its header block began with
// (ROB-010-003).
//
// An entity whose header block began with a UTF-16 or UTF-32 byte order mark
// was not read as a header (see textproto.Header.ByteOrderMark and bom.go) and
// is opaque under every entry point and profile: the media type is empty, not
// text/plain, the parameters nil, and the error an ordinary one saying why —
// neither a FatalError nor a warning, and not returned by Parse, which returns
// such an entity with a nil error — unless ProfileStrict refused it, which is
// reported as above. That holds for an entity New built from a copy of such a
// header too, whatever Content-Type the copy was given.
//
// The result is a snapshot from construction rather than a reading of the
// current header: an edit to Entity.Header after construction does not change
// it, just as it does not change the decoders already chosen. The parameter map
// is a fresh copy on every call. See the top of effective.go.
//
// An Entity that no constructor built — a struct literal — has no snapshot, and
// for it EffectiveContentType is Header.ContentType of its current header.
func (e *Entity) EffectiveContentType() (mediaType string, params map[string]string, err error) {
	if e.disposition == nil {
		return e.Header.ContentType()
	}
	return e.mediaType, maps.Clone(e.mediaParams), e.mediaErr
}

// EffectiveContentDisposition returns the disposition and parameters of the
// Content-Disposition this entity was constructed with, as the parse it came
// from interprets it, and the error that interpretation returned. It is what
// ReadOptions.SkipTextAttachmentDecoding decided an attachment's charset stage
// by, and what mail.Reader classifies a part by.
//
// For an entity from Parse the field is read under Limits.MaxParameterCount and
// Limits.MaxParameterSegments, on the same terms as EffectiveContentType. It is
// interpreted at most once: the first call to need it — the charset stage during
// construction, or the first call to this method — reads it and records any
// budget diagnostic, and every later call returns that same interpretation and
// records nothing. For an entity from New, Read or ReadWithOptions it is what
// Header.ContentDisposition returned for the header as it was at construction.
// A missing field is returned as Header.ContentDisposition returns one: an
// empty value, no parameters, and the error. Under Parse a field that is
// present is always read: one the whole-field parser refuses is repaired, or
// under ProfileStrict refused, on the terms EffectiveContentType describes. A
// field with parameters and no readable disposition type is an "attachment",
// as RFC 2183 section 2.8 has an unrecognized disposition treated, and raw
// 8-bit bytes in a parameter are tried in the charset of the entity's
// Content-Type. A repaired "attachment" is an attachment to the charset stage
// and to mail.Reader alike. Its diagnostics are recorded once, by the first
// reading.
//
// A strict refusal of the disposition differs from one of the Content-Type in
// what it can still prevent, because it is known only when the field is first
// read. Read by the charset stage during construction, it keeps that stage from
// running; transfer decoding, which does not depend on it, is already set up.
// Read later, by this method, it refuses the entity — mail.Reader and Walk
// report it — but the decoders chosen at construction stay, and bytes already
// read from Body stay read. Nothing reads the field early to avoid that.
//
// Like EffectiveContentType it is a construction snapshot rather than a reading
// of the current header, and the parameter map is a fresh copy on every call.
// Recording a budget diagnostic may let a DiagnosticSink stop the parse; the
// result is still returned, and ParseStop reports the stop. The result is
// published before that diagnostic is delivered, so the sink may call this
// method on the same entity and gets the same result; see "Reading from a
// DiagnosticSink" at the top of effective.go for what a sink may and may not
// do. A concurrent first call in another goroutine gets the same result too, but
// may get it before the delivery has finished.
//
// An Entity that no constructor built — a struct literal — has no snapshot, and
// for it EffectiveContentDisposition is Header.ContentDisposition of its current
// header.
func (e *Entity) EffectiveContentDisposition() (disp string, params map[string]string, err error) {
	if e.disposition == nil {
		return e.Header.ContentDisposition()
	}
	disp, params, err = e.disposition.get()
	return disp, maps.Clone(params), err
}

// ParseStop returns the failure that ended the whole parse this entity belongs
// to, or nil while that parse may continue. It is the identical value Parse,
// MultipartReader.NextPart and Entity.Walk report for that stop: a FatalError
// for a DiagnosticSink that asked the parse to stop, or for a budget whose scope
// is the whole parse, such as Limits.MaxPartCount or Limits.MaxInputBytes, or
// for an input that stalled (input.no_progress; see Parse).
//
// For a parse started by ParseContext, it also reports the cancellation of the
// parse's context once that context is done: the cancellation is then published
// as the stop, unless another stop was published first, which stays the one
// reported. The context's methods run with no lock held. See ParseContext.
//
// It reports only a stop of the whole parse. A failure confined to one scope —
// a child header past a budget, which ends one multipart iterator — is reported
// by that scope and not here. It offers no way to request a stop or to clear
// one. The one thing a call can write is a cancellation the caller already
// requested through the context of ParseContext: observing that the context is
// done publishes it, as any other check of the parse would, when no stop was
// published first. A parse whose context is live, or that has none, is not
// changed by calling it.
//
// It exists for a layer that iterates an entity without going through the
// parser's own scopes, as mail.Reader does for a message with a single part,
// so that such a layer can refuse to continue a parse that was stopped. An
// entity that does not come from Parse belongs to no parse that could be
// stopped, and ParseStop returns nil for it.
func (e *Entity) ParseStop() error {
	return e.parse.fatalErr()
}

// ParameterCandidates returns every candidate value of every parameter of the
// Content-Type or the Content-Disposition this entity was constructed with —
// field is one of those two names, in any case — in the order the field holds
// them. It returns nil for any other field, and for a field the entity does not
// have.
//
// EffectiveContentType and EffectiveContentDisposition return one value per
// parameter. A field that repeats a parameter, or gives it both as name and as
// name*, offers more than one, and none is discarded: each is a candidate here,
// with the raw text it was read from, and the one the entity uses is marked
// Chosen. A parameter whose candidates are never resolved by choosing — a
// boundary that repeats with different values — has candidates and no chosen
// one. See ParameterCandidate, and mediatype_tolerant.go for the precedence.
//
// The candidates are read tolerantly whatever the profile, under the parameter
// budgets of the parse the entity came from (the default budgets for an entity
// that came from no parse), so a field past a budget offers the candidates
// within it. They are a reading of their own, made on the first call and kept:
// it reads the field as it was at construction, records no diagnostic — the
// entity's construction already recorded what the field is owed — and may call
// the configured CharsetReader again for the values it decodes. The result is a
// fresh copy on every call.
func (e *Entity) ParameterCandidates(field string) []ParameterCandidate {
	var typeCands, dispCands []ParameterCandidate
	if e.disposition == nil {
		typeCands, dispCands = e.Header.parameterCandidates()
	} else {
		typeCands, dispCands = e.disposition.candidates()
	}
	switch {
	case strings.EqualFold(field, "Content-Type"):
		return slices.Clone(typeCands)
	case strings.EqualFold(field, "Content-Disposition"):
		return slices.Clone(dispCands)
	}
	return nil
}

// FilenameCandidates returns every filename this entity's header offers, the
// one to use first; see Header.FilenameCandidates for the sources, their
// precedence and what a candidate holds. It returns nil when the entity names
// no file.
//
// It differs from Header.FilenameCandidates in what it reads and what it
// records. It reads the Content-Disposition and Content-Type the entity was
// constructed with, under the parameter budgets of its parse, rather than the
// header as it is now. And the first call that finds candidates with different
// names records one mime.filename_conflict diagnostic, of SeverityInfo, in the
// parse's session: nothing was lost, since every candidate is returned, but a
// caller that only takes the first should know there was a choice. Later calls
// record nothing. Under ProfileStrict a field the parse refused offers no
// candidates.
//
// Reading the disposition for the first time records its diagnostics, as
// EffectiveContentDisposition does, so the same rule holds for a
// DiagnosticSink: it may call this for an entity whose filenames were already
// asked for, and must not be the first to ask, since recording from inside a
// delivery blocks (see "Reading from a DiagnosticSink" at the top of this
// file). The result is a fresh copy on every call.
func (e *Entity) FilenameCandidates() []FilenameCandidate {
	d := e.disposition
	if d == nil {
		return e.Header.FilenameCandidates()
	}
	// The disposition is interpreted first, with no lock of the candidates
	// held, so that its diagnostics are delivered as for any other reading.
	_, _, dispErr := d.get()
	typeCands, dispCands := d.candidates()
	if IsFatal(e.mediaErr) {
		typeCands = nil
	}
	if IsFatal(dispErr) {
		dispCands = nil
	}

	d.cmu.Lock()
	if !d.namesDone {
		d.names, d.namesDone = filenameCandidates(dispCands, typeCands), true
	}
	names := d.names
	note := !d.namesNoted && filenamesConflict(names)
	if note {
		d.namesNoted = true
	}
	d.cmu.Unlock()

	if note && d.ctx.fatalErr() == nil {
		_ = d.ctx.recordHeader(filenameConflictDiagnostic(names))
	}
	return slices.Clone(names)
}
