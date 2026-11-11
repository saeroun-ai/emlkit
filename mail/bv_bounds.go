package mail

import (
	"strconv"

	"github.com/saeroun-ai/emlkit"
)

// Bounds the tolerant accessors hold to whatever shape the value has
// (ROB-120-001; see docs/robustness-notes/bounds-audit-values.md).

// bvEntryLimit reports, once per call of the parser, that the address list
// holds more entries than maxTolerantEntries and that the rest is not parsed.
func (p *addrListParser) bvEntryLimit() {
	if p.bvLimited {
		return
	}
	p.bvLimited = true
	p.diags.add(message.CodeAddressPartialList, message.SeverityLimitExceeded,
		"mail: address list holds more than "+strconv.Itoa(maxTolerantEntries)+" entries",
		"kept the first "+strconv.Itoa(maxTolerantEntries)+" entries; the rest of the value was not parsed")
}

// bvBareWithin returns the identifiers without angle brackets that still fit
// under maxTolerantEntries beside the entries already read, and reports the
// ones that do not: entries and bare words share the one bound the result
// documents.
func (p *msgIDParser) bvBareWithin() []MsgIDEntry {
	room := max(maxTolerantEntries-len(p.entries), 0)
	if len(p.bare) <= room {
		return p.bare
	}
	if !p.limited {
		p.limited = true
		p.d.add(message.CodeMessageIDPartialList, message.SeverityLimitExceeded,
			"mail: field holds more than "+strconv.Itoa(maxTolerantEntries)+" message identifiers",
			"kept the first "+strconv.Itoa(maxTolerantEntries)+" entries; the other words holding \"@\" are not returned")
	}
	return p.bare[:room]
}
