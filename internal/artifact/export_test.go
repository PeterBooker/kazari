package artifact

import "github.com/PeterBooker/kazari/internal/csp"

// GCWaiting reports whether CollectGarbage waits on blobMu (a pending writer
// makes TryRLock fail), so a test can release a parked publish without
// sleeping. Only meaningful while a publish holds the read side.
func GCWaiting(s *Service) bool {
	if s.blobMu.TryRLock() {
		s.blobMu.RUnlock()
		return false
	}
	return true
}

// SplitLinkCookie is how CheckShareLink reads a view cookie.
var SplitLinkCookie = splitLinkCookie

// CheckPage runs the checks after a publish on one page, with the paths of
// the version's other files.
func CheckPage(policy string, override bool, page string, data []byte, paths ...string) []string {
	have := map[string]bool{page: true}
	for _, p := range paths {
		have[p] = true
	}
	c := checker{have: have, policy: csp.Parse(policy), checkCSP: policy != "", override: override, seen: map[string]bool{}}
	if checkKind(page) == "css" {
		c.css(page, page, data)
	} else {
		c.html(page, data)
	}
	return c.out
}
