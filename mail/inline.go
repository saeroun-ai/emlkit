package mail

import (
	"github.com/saeroun-ai/emlkit"
)

// A InlineHeader represents a message text header.
type InlineHeader struct {
	message.Header
}

var _ PartHeader = (*InlineHeader)(nil)

func (*InlineHeader) partHeader() {}
