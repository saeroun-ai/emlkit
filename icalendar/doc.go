// Package icalendar reads iCalendar objects (RFC 5545) as they arrive in
// mail (RFC 6047): invitations, updates, cancellations and replies (RFC
// 5546). It is optional; the rest of the module does not import it.
//
// It has two layers. ParseRaw gives the raw layer: content lines unfolded
// and split into name, parameters and value, and the BEGIN/END component
// tree, with every property in order and its raw line kept. It interprets no
// value, so it is the fallback for anything the convenience layer does not
// model. ParseBytes and Parse add the convenience layer on top: a Calendar
// with its events, to-dos and time zones, TEXT values unescaped, date-times
// resolved to instants where the zone is known, addresses normalised.
// Classify and Compare are the scheduling helpers.
//
// Damaged input is healed, never refused: lines without a colon, missing or
// misplaced ENDs, content around the calendar, bare line endings, folds that
// split UTF-8 sequences and more are repaired, and each repair is an Issue.
// A date-time whose zone cannot be resolved keeps its wall clock and TZID
// and says that its instant is unknown (TimeUnknownZone); it is not guessed.
// Every budget of Limits holds whatever the input is; hitting one keeps what
// was read before it.
//
// Time zones resolve through time.LoadLocation, so they need the host's zone
// database, or a program that imports time/tzdata.
package icalendar
