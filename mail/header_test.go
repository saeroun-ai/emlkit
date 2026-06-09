package mail_test

import (
	"bufio"
	"bytes"
	netmail "net/mail"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit/mail"
)

func TestHeader(t *testing.T) {
	date := time.Unix(1466253744, 0)
	from := []*mail.Address{{"Mitsuha Miyamizu", "mitsuha.miyamizu@example.org"}}
	subject := "Café"

	var h mail.Header
	h.SetAddressList("From", from)
	h.SetDate(date)
	h.SetSubject(subject)

	if got, err := h.Date(); err != nil {
		t.Error("Expected no error while parsing header date, got:", err)
	} else if !got.Equal(date) {
		t.Errorf("Expected header date to be %v, but got %v", date, got)
	}

	if got, err := h.AddressList("From"); err != nil {
		t.Error("Expected no error while parsing header address list, got:", err)
	} else if !reflect.DeepEqual(got, from) {
		t.Errorf("Expected header address list to be %v, but got %v", from, got)
	}

	if got, err := h.AddressList("Cc"); err != nil {
		t.Error("Expected no error while parsing missing header address list, got:", err)
	} else if got != nil {
		t.Errorf("Expected missing header address list to be %v, but got %v", nil, got)
	}

	if got, err := h.Subject(); err != nil {
		t.Error("Expected no error while parsing header subject, got:", err)
	} else if got != subject {
		t.Errorf("Expected header subject to be %v, but got %v", subject, got)
	}
}

func TestHeader_Date_CFWS(t *testing.T) {
	tc := []string{
		"Mon, 22 Jul 2019 13:57:29 -0500 (GMT-05:00)",
		"Mon, 22 Jul 2019 13:57:29 -0500",
		"Mon, 2 Jan 06 15:04:05 MST (Some random stuff)",
		"Mon, 2 Jan 06 15:04:05 MST",
	}
	var h mail.Header
	for _, tt := range tc {
		h.Set("Date", tt)
		_, err := h.Date()
		if err != nil {
			t.Errorf("Failed to parse time %q: %v", tt, err)
		}
	}
}

func TestHeader_Date_empty(t *testing.T) {
	var h mail.Header
	date, err := h.Date()
	if err != nil {
		t.Errorf("Date() = %v", err)
	} else if !date.IsZero() {
		t.Errorf("Date() = %v, want time.Time{}", date)
	}
}

func TestHeader_MessageID(t *testing.T) {
	tests := []struct {
		raw   string
		msgID string
	}{
		{"", ""},
		{"<123@asdf>", "123@asdf"},
		{
			"  \t <DM6PR09MB253761A38B42C713082A7CE2C60C0@DM6PR09MB2537.namprd09.prod.outlook.com>",
			"DM6PR09MB253761A38B42C713082A7CE2C60C0@DM6PR09MB2537.namprd09.prod.outlook.com",
		},
		{
			`<20200122161125.7enac4n5rsxfnhg7@example.com> (Christopher Wellons's message of "Wed, 22 Jan 2020 11:11:25 -0500")`,
			"20200122161125.7enac4n5rsxfnhg7@example.com",
		},
		{
			"<123@[2001:db8:85a3:8d3:1319:8a2e:370:7348]>",
			"123@[2001:db8:85a3:8d3:1319:8a2e:370:7348]",
		},
	}
	for _, test := range tests {
		var h mail.Header
		h.Set("Message-ID", test.raw)
		msgID, err := h.MessageID()
		if err != nil {
			t.Errorf("Failed to parse Message-ID %q: Header.MessageID() = %v", test.raw, err)
		} else if msgID != test.msgID {
			t.Errorf("Failed to parse Message-ID %q: Header.MessageID() = %q, want %q", test.raw, msgID, test.msgID)
		}
	}
}

func TestHeader_MsgIDList(t *testing.T) {
	tests := []struct {
		raw    string
		msgIDs []string
	}{
		{"", nil},
		{"<123@asdf>", []string{"123@asdf"}},
		{
			"  \t <DM6PR09MB253761A38B42C713082A7CE2C60C0@DM6PR09MB2537.namprd09.prod.outlook.com>",
			[]string{"DM6PR09MB253761A38B42C713082A7CE2C60C0@DM6PR09MB2537.namprd09.prod.outlook.com"},
		},
		{
			`<20200122161125.7enac4n5rsxfnhg7@example.com> (Christopher Wellons's message of "Wed, 22 Jan 2020 11:11:25 -0500")`,
			[]string{"20200122161125.7enac4n5rsxfnhg7@example.com"},
		},
		{
			"<87pnfb69f3.fsf@bernat.ch>  \t <20200122161125.7enac4n5rsxfnhg7@nullprogram.com>",
			[]string{"87pnfb69f3.fsf@bernat.ch", "20200122161125.7enac4n5rsxfnhg7@nullprogram.com"},
		},
		{
			"<87pnfb69f3.fsf@bernat.ch> (a comment) \t <20200122161125.7enac4n5rsxfnhg7@nullprogram.com> (another comment)",
			[]string{"87pnfb69f3.fsf@bernat.ch", "20200122161125.7enac4n5rsxfnhg7@nullprogram.com"},
		},
	}
	for _, test := range tests {
		var h mail.Header
		h.Set("In-Reply-To", test.raw)
		msgIDs, err := h.MsgIDList("In-Reply-To")
		if err != nil {
			t.Errorf("Failed to parse In-Reply-To %q: Header.MsgIDList() = %v", test.raw, err)
		} else if !reflect.DeepEqual(msgIDs, test.msgIDs) {
			t.Errorf("Failed to parse In-Reply-To %q: Header.MsgIDList() = %q, want %q", test.raw, msgIDs, test.msgIDs)
		}
	}
}

func TestHeader_GenerateMessageID(t *testing.T) {
	var h mail.Header
	if err := h.GenerateMessageID(); err != nil {
		t.Fatalf("Header.GenerateMessageID() = %v", err)
	}
	if _, err := h.MessageID(); err != nil {
		t.Errorf("Failed to parse generated Message-Id: Header.MessageID() = %v", err)
	}
}

func TestHeader_SetMsgIDList(t *testing.T) {
	tests := []struct {
		raw    string
		msgIDs []string
	}{
		{"", nil},
		{"<123@asdf>", []string{"123@asdf"}},
		{"<123@asdf> <456@asdf>", []string{"123@asdf", "456@asdf"}},
	}
	for _, test := range tests {
		var h mail.Header
		h.SetMsgIDList("In-Reply-To", test.msgIDs)
		raw := h.Get("In-Reply-To")
		if raw != test.raw {
			t.Errorf("Failed to format In-Reply-To %q: Header.Get() = %q, want %q", test.msgIDs, raw, test.raw)
		}
	}
}

func TestHeader_CanUseNetMailAddress(t *testing.T) {
	netfrom := []*netmail.Address{{"Mitsuha Miyamizu", "mitsuha.miyamizu@example.org"}}
	mailfrom := []*mail.Address{{"Mitsuha Miyamizu", "mitsuha.miyamizu@example.org"}}

	//sanity check that they types are identical
	if !reflect.DeepEqual(netfrom, mailfrom) {
		t.Error("[]*net/mail.Address differs from []*mail.Address")
	}

	//roundtrip
	var h mail.Header
	h.SetAddressList("From", netfrom)
	if got, err := h.AddressList("From"); err != nil {
		t.Error("Expected no error while parsing header address list, got:", err)
	} else if !reflect.DeepEqual(got, netfrom) {
		t.Errorf("Expected header address list to be %v, but got %v", netfrom, got)
	}
}

func TestHeader_EmptyAddressList(t *testing.T) {
	tests := []struct {
		key   string
		list  []*mail.Address
		unset bool
	}{
		{"cc", nil, false},
		{"to", []*mail.Address{}, false},
		{"cc", []*mail.Address{{"Mitsuha Miyamizu", "mitsuha.miyamizu@example.org"}}, true},
	}

	for _, test := range tests {
		var h mail.Header
		h.SetAddressList(test.key, test.list)
		if test.unset {
			h.SetAddressList(test.key, nil)
		}
		buf := bytes.NewBuffer(nil)
		w, err := mail.CreateSingleInlineWriter(buf, h)
		if err != nil {
			t.Error("Expected no error while creating inline writer, got:", err)
		}
		if err := w.Close(); err != nil {
			t.Error("Expected no error while closing inline writer, got:", err)
		}
		scanner := bufio.NewScanner(buf)
		for scanner.Scan() {
			line := strings.ToLower(scanner.Text())
			if strings.HasPrefix(line, test.key) {
				t.Error("Expected no address list header field, but got:", scanner.Text())
			}
		}
	}
}

func TestHeader_ListCommandURLList(t *testing.T) {
	var h mail.Header
	h.Set("List-Unsubscribe", "<https://example.com/unsub>, <mailto:unsub@example.com>")

	urls, err := h.ListCommandURLList("List-Unsubscribe")
	if err != nil {
		t.Fatal("ListCommandURLList:", err)
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d (%v)", len(urls), urls)
	}
	if got := urls[0].String(); got != "https://example.com/unsub" {
		t.Errorf("urls[0] = %q", got)
	}
	if got := urls[1].String(); got != "mailto:unsub@example.com" {
		t.Errorf("urls[1] = %q", got)
	}

	// round-trip
	var h2 mail.Header
	h2.SetListCommandURLList("List-Unsubscribe", urls)
	urls2, err := h2.ListCommandURLList("List-Unsubscribe")
	if err != nil {
		t.Fatal("round-trip ListCommandURLList:", err)
	}
	if len(urls2) != 2 || urls2[0].String() != urls[0].String() || urls2[1].String() != urls[1].String() {
		t.Errorf("round-trip mismatch: %v", urls2)
	}
}

func TestHeader_ListCommand_NO_and_malformed(t *testing.T) {
	// List-Post의 특수값 "NO"는 [nil]로 파싱된다.
	var h mail.Header
	h.Set("List-Post", "NO")
	urls, err := h.ListCommandURLList("List-Post")
	if err != nil {
		t.Fatal("ListCommandURLList(NO):", err)
	}
	if len(urls) != 1 || urls[0] != nil {
		t.Errorf("List-Post NO: expected [nil], got %v", urls)
	}
	// [nil]을 다시 Set해도 panic하지 않아야 한다 (nil 가드).
	var h2 mail.Header
	h2.SetListCommandURLList("List-Post", urls)

	// 결함 ⓑ: '>'가 없는 malformed 입력은 panic하지 않고 에러를 반환해야 한다.
	var hm mail.Header
	hm.Set("List-Help", "<https://example.com/help")
	if _, err := hm.ListCommandURLList("List-Help"); err == nil {
		t.Error("malformed list command: expected error, got nil")
	}
}

func TestHeader_SetListCommandURLList_empty(t *testing.T) {
	// 결함 ⓐ: 빈 슬라이스/nil이면 헤더를 삭제해야 한다 ("<>"를 쓰면 안 됨).
	var h mail.Header
	h.Set("List-Help", "<https://example.com/help>")
	h.SetListCommandURLList("List-Help", nil)
	if v := h.Get("List-Help"); v != "" {
		t.Errorf("expected List-Help removed, got %q", v)
	}
}
