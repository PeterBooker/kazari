//go:build !unix

package backup

type owner struct{}

func ownerOf(string) (owner, error) { return owner{}, nil }

func (owner) apply(string) error { return nil }
