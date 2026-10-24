package cli

import (
	"flag"
	"io"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseInterspersed(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		max     int
		wantPos []string
		wantX   string
		wantErr bool
	}{
		{"flags after", []string{"a", "-x", "1", "b"}, -1, []string{"a", "b"}, "1", false},
		{"flags before", []string{"--x=2", "a"}, -1, []string{"a"}, "2", false},
		{"double dash", []string{"a", "--", "-x", "b"}, -1, []string{"a", "-x", "b"}, "", false},
		{"too many", []string{"a", "b"}, 1, nil, "", true},
		{"none", nil, 0, nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			x := fs.String("x", "", "")
			pos, err := parse(fs, tt.args, 0, tt.max)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr {
				return
			}
			if diff := cmp.Diff(tt.wantPos, pos); diff != "" || *x != tt.wantX {
				t.Errorf("x = %q, positionals (-want +got):\n%s", *x, diff)
			}
		})
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"}, {1023, "1023 B"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"},
		{5 << 20, "5.0 MiB"}, {3 << 30, "3.0 GiB"}, {2 << 40, "2.0 TiB"},
	}
	for _, tt := range tests {
		if got := humanSize(tt.n); got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false}, {"1048576", 1 << 20, false}, {"500MiB", 500 << 20, false},
		{"1.5 GiB", 3 << 29, false}, {"2gb", 2e9, false}, {"10k", 10 << 10, false}, {"7B", 7, false},
		{"lots", 0, true}, {"-1", 0, true}, {"MiB", 0, true},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d, err %t", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}
