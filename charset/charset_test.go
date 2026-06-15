package charset

import (
	"bytes"
	"io/ioutil"
	"strings"
	"testing"
)

var testCharsets = []struct {
	charset string
	encoded []byte
	decoded string
}{
	{
		charset: "us-ascii",
		encoded: []byte("yuudachi"),
		decoded: "yuudachi",
	},
	{
		charset: "utf-8",
		encoded: []byte("café"),
		decoded: "café",
	},
	{
		charset: "utf8",
		encoded: []byte("café"),
		decoded: "café",
	},
	{
		charset: "windows-1250",
		encoded: []byte{0x8c, 0x8d, 0x8f, 0x9c, 0x9d, 0x9f, 0xbc, 0xbe},
		decoded: "ŚŤŹśťźĽľ",
	},
	{
		charset: "windows-1252",
		encoded: []byte{0x63, 0x61, 0x66, 0xE9, 0x20, 0x80},
		decoded: "café €",
	},
	{
		charset: "iso-8859-1",
		encoded: []byte{0x63, 0x61, 0x66, 0xE9},
		decoded: "café",
	},
	{
		charset: "idontexist",
		encoded: []byte{42},
	},
	{
		charset: "gb2312",
		encoded: []byte{178, 226, 202, 212},
		decoded: "测试",
	},
	{
		charset: "iso8859-2",
		encoded: []byte{0x63, 0x61, 0x66, 0xE9, 0x20, 0xfb},
		decoded: "café ű",
	},
}

func TestCharsetReader(t *testing.T) {
	for _, test := range testCharsets {
		r, err := Reader(test.charset, bytes.NewReader(test.encoded))
		if test.decoded == "" {
			if err == nil {
				t.Errorf("Expected an error when creating reader for charset %q", test.charset)
			}
		}
		if test.decoded != "" {
			if err != nil {
				t.Errorf("Expected no error when creating reader for charset %q, but got: %v", test.charset, err)
			} else if b, err := ioutil.ReadAll(r); err != nil {
				t.Errorf("Expected no error when reading charset %q, but got: %v", test.charset, err)
			} else if s := string(b); s != test.decoded {
				t.Errorf("Expected decoded text to be %q but got %q", test.decoded, s)
			}
		}
	}
}

func TestDisabledCharsetReader(t *testing.T) {
	charsets["DISABLED"] = nil

	_, err := Reader("DISABLED", strings.NewReader("Some dummy text"))
	if err == nil {
		t.Errorf("Reader(): expected disabled charset to return an error")
	}
}

func TestReader_utf7IsStandardUTF7(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "outlook style shifted ascii",
			in:   "+ADw-script+AD4-",
			want: "<script>",
		},
		{
			name: "non ascii",
			in:   "caf+AOk-",
			want: "café",
		},
		{
			name: "literal plus",
			in:   "+- +AOk-",
			want: "+ é",
		},
		{
			name: "standard base64 slash",
			in:   "+/v8-",
			want: "\ufeff",
		},
		{
			name: "standard base64 plus",
			in:   "++/8-",
			want: "\ufbff",
		},
		{
			name: "ampersand is direct",
			in:   "&AOk-",
			want: "&AOk-",
		},
		{
			name: "comma is not modified base64",
			in:   "abc &,v8-",
			want: "abc &,v8-",
		},
		{
			name: "direct terminator",
			in:   "+AOk!",
			want: "é!",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, err := Reader("utf-7", strings.NewReader(test.in))
			if err != nil {
				t.Fatalf("Reader(): unexpected error: %v", err)
			}
			b, err := ioutil.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll(): unexpected error: %v", err)
			}
			if got := string(b); got != test.want {
				t.Fatalf("Reader() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReader_utf7PreservesInvalidShift(t *testing.T) {
	r, err := Reader("utf-7", strings.NewReader("+A"))
	if err != nil {
		t.Fatalf("Reader(): unexpected setup error: %v", err)
	}
	b, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll(): unexpected error: %v", err)
	}
	if got := string(b); got != "+A" {
		t.Fatalf("Reader() = %q, want %q", got, "+A")
	}
}
