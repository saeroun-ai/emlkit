package mail

import (
	"strings"
)

// Format=flowed (RFC 3676; corpus finding provider-mobile-format-flowed).
//
// A text/plain body with format=flowed is sent with its paragraphs broken into
// short lines; a line that ends with a space is "flowed": the line after it
// continues the same paragraph. Bodies are delivered as sent, so a reader that
// tokenizes them sees words the sender's client split. UnwrapFlowed joins the
// lines back into paragraphs; Node.FlowedText applies it to a node that says it
// is flowed.

// UnwrapFlowed returns text, a format=flowed body as it was decoded, with each
// paragraph of flowed lines joined into one line (RFC 3676 section 4.2):
//
//   - A line's quote depth is the number of '>' it begins with. Its content is
//     what follows them, with one leading space removed: space-stuffing
//     (section 4.4), which after quote marks is also the space a quoting
//     client writes.
//   - A line whose content ends with a space is flowed, and the next line of
//     the same quote depth continues it. With delSp, the DelSp=yes parameter,
//     that space is deleted when the lines are joined (section 4.2), so a word
//     the sender split comes back whole; without it, it stays between them.
//   - A flowed line followed by a line of another quote depth, or by the end
//     of the text, ends its paragraph as if it were fixed (section 4.5).
//   - The signature separator "-- " is never flowed (section 4.3): it ends an
//     open paragraph and stands on a line of its own.
//
// Each paragraph is written with its quote marks followed by one space when
// it is quoted and not empty, and ended by the line ending of its last line:
// "\r\n" or "\n", as the text had it, or none for an unended last line. A
// bare '\r' is content. Text that is not flowed passes through with only its
// space-stuffing and quote spacing changed.
func UnwrapFlowed(text string, delSp bool) string {
	var out strings.Builder
	out.Grow(len(text))

	var para strings.Builder
	open, depth, end := false, 0, ""
	emit := func() {
		out.WriteString(strings.Repeat(">", depth))
		if depth > 0 && para.Len() > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(para.String())
		out.WriteString(end)
		para.Reset()
		open = false
	}

	for rest := text; rest != ""; {
		line, ending := rest, ""
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
			ending = "\n"
			if strings.HasSuffix(line, "\r") {
				line, ending = line[:len(line)-1], "\r\n"
			}
		} else {
			rest = ""
		}

		d := 0
		for d < len(line) && line[d] == '>' {
			d++
		}
		content := line[d:]
		if strings.HasPrefix(content, " ") {
			content = content[1:]
		}
		sig := content == "-- "
		flowed := !sig && strings.HasSuffix(content, " ")

		if open && (d != depth || sig) {
			// A flowed line that nothing of its depth continues is fixed.
			emit()
		}
		if !open {
			depth = d
		}
		if flowed && delSp {
			content = content[:len(content)-1]
		}
		para.WriteString(content)
		end = ending
		open = true
		if !flowed || ending == "" {
			emit()
		}
	}
	if open {
		emit()
	}
	return out.String()
}

// FlowedText returns the body of a text/plain node whose Content-Type says
// format=flowed, unwrapped by UnwrapFlowed with the DelSp its delsp parameter
// says, and true; or "" and false for any other node. It unwraps Data, so a
// Truncated node gives the paragraphs of what was kept.
func (n *Node) FlowedText() (string, bool) {
	if n == nil || n.MediaType != "text/plain" || !strings.EqualFold(strings.TrimSpace(n.Params["format"]), "flowed") {
		return "", false
	}
	delSp := strings.EqualFold(strings.TrimSpace(n.Params["delsp"]), "yes")
	return UnwrapFlowed(string(n.Data), delSp), true
}
