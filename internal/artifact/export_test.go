package artifact

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
