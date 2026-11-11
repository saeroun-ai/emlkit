package scaletest

import (
	"strconv"
	"strings"
)

// ValueUnits are short pieces of header-value syntax. Repeated to a length,
// each makes a value in which one construct — a comment, a quoted string, a
// parameter, an encoded word, a list item — is opened, nested or left
// unfinished as often as the length allows, which is where a parser that
// looks back or ahead from each one costs the square of the length.
var ValueUnits = []string{
	"a", " ", "\r\n ", ",", ";", "@", "=",
	"(", ")", "(\\", "\"", "\\",
	"<", ">", "<a@b> ", "a@b, ", "g:", "[",
	"; a=\"", "; a*0=b", "; a*1*=%41",
	"=?utf-8?q?", "=?utf-8?q?a?= =?",
	"from a by b ", "a=pass (c) b.d=e ", "\x80",
}

// NumberedFormats are items that differ from each other by a number, written
// '#': parameters, continuations and list items that repetition alone cannot
// make distinct.
var NumberedFormats = []string{
	"; p#=v", "; filename*#*=%41", "a#@example.com, ", "<id#@example.com> ", "; m#=pass header.d=d#",
}

// Value returns unit repeated to about n bytes, as one header value.
func Value(unit string, n int) string {
	return strings.Repeat(unit, max(1, n/len(unit)))
}

// Numbered returns about n bytes of the items format makes of 0, 1, 2 and so
// on.
func Numbered(format string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString(strings.ReplaceAll(format, "#", strconv.Itoa(i)))
	}
	return b.String()
}

// A ValueCase is one value to scale: Build returns it at about n bytes.
type ValueCase struct {
	Name  string
	Build func(n int) string
}

// ValueCases returns every unit and every numbered format as a case.
func ValueCases() []ValueCase {
	var cases []ValueCase
	for _, u := range ValueUnits {
		cases = append(cases, ValueCase{strconv.Quote(u), func(n int) string { return Value(u, n) }})
	}
	for _, f := range NumberedFormats {
		cases = append(cases, ValueCase{"numbered " + strconv.Quote(f), func(n int) string { return Numbered(f, n) }})
	}
	return cases
}
