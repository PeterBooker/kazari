package present_test

import (
	"errors"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/present"
)

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{"72h", "14d", "2w", "1w2d3h", "1d-2h", "12h1d", "0d", "99999999999w", " 90m ", "d", "w1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := present.ParseDuration("grace", s)
		if err != nil {
			if !errors.Is(err, artifact.ErrInvalidArgument) {
				t.Errorf("ParseDuration(%q) = %v, want invalid_argument", s, err)
			}
			return
		}
		if d < 0 {
			t.Errorf("ParseDuration(%q) = %v, want no negative duration", s, d)
		}
	})
}
