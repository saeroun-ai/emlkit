package mail_test

import (
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// TestUnwrapFlowed pins RFC 3676 unwrapping: soft and hard breaks, DelSp,
// space-stuffing, quote depth and the signature separator.
func TestUnwrapFlowed(t *testing.T) {
	tests := []struct {
		name, in string
		delSp    bool
		want     string
	}{
		// Section 4.2: a line ending in a space is flowed into the next.
		{"soft breaks", "This is a paragraph that \r\nis flowed across \r\nthree lines.\r\n", false,
			"This is a paragraph that is flowed across three lines.\r\n"},
		{"hard break", "One line.\r\nAnother line.\r\n", false, "One line.\r\nAnother line.\r\n"},
		{"paragraphs", "First \r\nparagraph.\r\n\r\nSecond \r\none.\r\n", false, "First paragraph.\r\n\r\nSecond one.\r\n"},
		{"LF line endings", "a \nb\nc\n", false, "a b\nc\n"},
		{"unended last line", "a \r\nb", false, "a b"},
		{"flowed last line", "a \r\nb ", false, "a b "},
		// Section 4.2: DelSp=yes deletes the space of each soft break.
		{"delsp joins a split word", "만나 \r\n요.\r\n", true, "만나요.\r\n"},
		{"delsp keeps a second space", "연락  \r\n주세요.\r\n", true, "연락 주세요.\r\n"},
		{"delsp=no keeps the space", "만나 \r\n요.\r\n", false, "만나 요.\r\n"},
		// Section 4.4: one leading space is stuffing.
		{"space-stuffed From", " From the archive \r\nof the list.\r\n", false, "From the archive of the list.\r\n"},
		{"space-stuffed space", "  indented\r\n", false, " indented\r\n"},
		// Section 4.5: quote depth; a change of depth ends a paragraph.
		{"quoted paragraph", "> quoted text \r\n> continues.\r\n", false, "> quoted text continues.\r\n"},
		{"nested quotes", ">> deep \r\n>> deeper\r\n> back\r\n", false, ">> deep deeper\r\n> back\r\n"},
		{"depth change ends a flowed line", "> quoted \r\nnot quoted\r\n", false, "> quoted \r\nnot quoted\r\n"},
		{"quote marks without space", ">a \r\n>b\r\n", false, "> a b\r\n"},
		{"empty quoted line", ">\r\n", false, ">\r\n"},
		// Section 4.3: the signature separator is never flowed.
		{"signature separator", "Thanks \r\n-- \r\nAlice\r\n", false, "Thanks \r\n-- \r\nAlice\r\n"},
		{"signature separator delsp", "-- \r\nSent from a phone\r\n", true, "-- \r\nSent from a phone\r\n"},
		{"empty", "", false, ""},
	}
	for _, tt := range tests {
		if got := mail.UnwrapFlowed(tt.in, tt.delSp); got != tt.want {
			t.Errorf("%s: UnwrapFlowed(%q, %v) = %q, want %q", tt.name, tt.in, tt.delSp, got, tt.want)
		}
	}
}

// TestNode_FlowedText reads the corpus's format=flowed shape through the tree
// (corpus finding provider-mobile-format-flowed).
func TestNode_FlowedText(t *testing.T) {
	raw := "Content-Type: text/plain; charset=utf-8; format=flowed; delsp=yes\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		"오늘 점심은 12시 반에 회사 앞 식당에서 만나 \r\n요. 늦으면 미리 연락  \r\n주세요.\r\n\r\n" +
		"> 내일 점심 같이 하실래요? 메뉴는 아무거나  \r\n> 좋아요.\r\n\r\n-- \r\nSent from ExampleMail for Android\r\n"
	_, e, err := message.Parse(strings.NewReader(raw), nil)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	tree, err := mail.ReadTree(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tree.Root.FlowedText()
	want := "오늘 점심은 12시 반에 회사 앞 식당에서 만나요. 늦으면 미리 연락 주세요.\r\n\r\n" +
		"> 내일 점심 같이 하실래요? 메뉴는 아무거나 좋아요.\r\n\r\n-- \r\nSent from ExampleMail for Android\r\n"
	if !ok || got != want {
		t.Errorf("FlowedText() = %q, %v\nwant %q", got, ok, want)
	}

	_, e, _ = message.Parse(strings.NewReader("Content-Type: text/plain\r\n\r\na \r\nb\r\n"), nil)
	tree, _ = mail.ReadTree(e, nil)
	if got, ok := tree.Root.FlowedText(); ok || got != "" {
		t.Errorf("fixed text: FlowedText() = %q, %v", got, ok)
	}
}
