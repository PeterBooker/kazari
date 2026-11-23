package intake_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"testing"

	"github.com/PeterBooker/kazari/internal/intake"
)

// FuzzDecode holds Decode to what it may do with any content: never panic,
// return text as it is, and read back the bytes of base64 however it is
// sent, padded, unpadded, broken into lines or as a data: URL.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{"", "hi", "aGk=", "aGk", "data:x;base64,aGk=", " a G k ", "\x00\xff", "===="} {
		f.Add([]byte(s), "base64")
		f.Add([]byte(s), "")
	}
	f.Fuzz(func(t *testing.T, data []byte, encoding string) {
		if r, err := intake.Decode("p", string(data), encoding); err == nil {
			if _, err := io.ReadAll(r); err != nil {
				t.Fatalf("Decode(p, %q, %q) reads with %v", data, encoding, err)
			}
		}
		text := decoded(t, string(data), "utf8")
		if !bytes.Equal(text, data) {
			t.Errorf("Decode(p, %q, utf8) = %q, want it unchanged", data, text)
		}
		padded := base64.StdEncoding.EncodeToString(data)
		for _, sent := range []string{
			padded,
			base64.RawStdEncoding.EncodeToString(data),
			"data:application/octet-stream;base64," + padded,
			lines(padded),
		} {
			if got := decoded(t, sent, "base64"); !bytes.Equal(got, data) {
				t.Errorf("Decode(p, %q, base64) = %q, want %q", sent, got, data)
			}
		}
	})
}

func decoded(t *testing.T, content, encoding string) []byte {
	t.Helper()
	r, err := intake.Decode("p", content, encoding)
	if err != nil {
		t.Fatalf("Decode(p, %q, %q) = %v", content, encoding, err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("Decode(p, %q, %q) reads with %v", content, encoding, err)
	}
	return b
}

// lines breaks s as a mail client would, with a space and a tab for good
// measure.
func lines(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i += 4 {
		b.WriteString(s[i:min(i+4, len(s))])
		b.WriteString([]string{"\r\n", " ", "\t", "\n"}[i/4%4])
	}
	return b.String()
}
