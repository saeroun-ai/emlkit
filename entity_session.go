package message

// ParseSession returns the session of the parse e came from — the session
// Parse returned with the message, shared by every entity of it — or nil for
// an entity that no session saw: one of Read, ReadWithOptions, New or
// NewMultipart, which record no diagnostics. It lets a layer that is handed
// only an entity, such as mail.ReadTree, read the parse's diagnostics and
// Completion. A session offers no way to record or clear anything.
func (e *Entity) ParseSession() *ParseSession {
	if e == nil {
		return nil
	}
	return e.parse.session
}
