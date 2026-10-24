package artifact

// GCWaiting reports whether CollectGarbage is waiting for the publishes
// that hold the read side of blobMu (a pending writer makes TryRLock fail).
// It lets a test release a parked publish at exactly that point without
// sleeping. It is only meaningful while a publish holds the read side.
func GCWaiting(s *Service) bool {
	if s.blobMu.TryRLock() {
		s.blobMu.RUnlock()
		return false
	}
	return true
}
