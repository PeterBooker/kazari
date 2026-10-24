package site

import "testing"

func TestParseHost(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		base        string
		wantSlug    string
		wantVersion int
		wantOK      bool
	}{
		{"latest", "demo.art.test", "art.test", "demo", 0, true},
		{"version", "demo--v3.art.test", "art.test", "demo", 3, true},
		{"large version", "demo--v123456789.art.test", "art.test", "demo", 123456789, true},
		{"port", "demo.art.test:8443", "art.test", "demo", 0, true},
		{"version and port", "demo--v12.art.test:80", "art.test", "demo", 12, true},
		{"upper case host", "DeMo--V2.ART.Test", "art.test", "demo", 2, true},
		{"upper case base", "demo.art.test", "ART.TEST", "demo", 0, true},
		{"trailing dot", "demo.art.test.", "art.test", "demo", 0, true},
		{"hyphenated slug", "my-demo-1--v4.art.test", "art.test", "my-demo-1", 4, true},
		{"slug ending in v digits", "nav2.art.test", "art.test", "nav2", 0, true},
		{"reserved label parses", "api.art.test", "art.test", "api", 0, true},

		{"apex", "art.test", "art.test", "", 0, false},
		{"apex with port", "art.test:443", "art.test", "", 0, false},
		{"empty label", ".art.test", "art.test", "", 0, false},
		{"version zero", "demo--v0.art.test", "art.test", "", 0, false},
		{"leading zero", "demo--v03.art.test", "art.test", "", 0, false},
		{"too many digits", "demo--v1234567890.art.test", "art.test", "", 0, false},
		{"non-numeric version", "demo--vx.art.test", "art.test", "", 0, false},
		{"empty version", "demo--v.art.test", "art.test", "", 0, false},
		{"digits then letters", "demo--v1x.art.test", "art.test", "", 0, false},
		{"double hyphen", "de--mo.art.test", "art.test", "", 0, false},
		{"double hyphen with version", "de--mo--v2.art.test", "art.test", "", 0, false},
		{"two versions", "demo--v1--v2.art.test", "art.test", "", 0, false},
		{"version only", "--v2.art.test", "art.test", "", 0, false},
		{"short slug", "ab.art.test", "art.test", "", 0, false},
		{"leading hyphen", "-demo.art.test", "art.test", "", 0, false},
		{"trailing hyphen", "demo-.art.test", "art.test", "", 0, false},
		{"underscore", "de_mo.art.test", "art.test", "", 0, false},
		{"extra label", "a.demo.art.test", "art.test", "", 0, false},
		{"wrong base", "demo.other.test", "art.test", "", 0, false},
		{"base as suffix of label", "demoart.test", "art.test", "", 0, false},
		{"empty base", "demo.art.test", "", "", 0, false},
		{"bad port syntax", "demo.art.test:1:2", "art.test", "", 0, false},
		{"ipv6", "[::1]:8080", "art.test", "", 0, false},
		{"empty host", "", "art.test", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slug, version, ok := ParseHost(tt.host, tt.base)
			if slug != tt.wantSlug || version != tt.wantVersion || ok != tt.wantOK {
				t.Errorf("ParseHost(%q, %q) = (%q, %d, %v), want (%q, %d, %v)",
					tt.host, tt.base, slug, version, ok, tt.wantSlug, tt.wantVersion, tt.wantOK)
			}
		})
	}
}
