package bytesize_test

import (
	"testing"

	"github.com/PeterBooker/kazari/internal/bytesize"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"}, {1023, "1023 B"}, {1024, "1 KiB"}, {1536, "1.5 KiB"}, {25 << 20, "25 MiB"}, {53477376, "51 MiB"},
		{3 << 30, "3 GiB"}, {1<<20 - 1, "1024 KiB"}, {50 << 40, "50 TiB"}, {2 << 50, "2 PiB"}, {1 << 62, "4096 PiB"},
	}
	for _, tt := range tests {
		if got := bytesize.Format(tt.n); got != tt.want {
			t.Errorf("Format(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
