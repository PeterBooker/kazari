package artifact

// DefaultMaxChunkBytes is the largest upload chunk, after any transport
// decoding.
const DefaultMaxChunkBytes = 4 << 20

// Limits are the sizes a version is held to, the same whichever way its
// files arrive: in one request, through an upload or from a page snapshot.
// Zero fields take the Default* values.
type Limits struct {
	MaxVersionBytes int64 // all files of one version, and of one upload
	MaxFileBytes    int64
	MaxFiles        int
	MaxChunkBytes   int64 // one upload chunk
}

// WithDefaults returns l with each zero field at its default.
func (l Limits) WithDefaults() Limits {
	if l.MaxVersionBytes <= 0 {
		l.MaxVersionBytes = DefaultMaxVersionBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	if l.MaxChunkBytes <= 0 {
		l.MaxChunkBytes = DefaultMaxChunkBytes
	}
	return l
}
