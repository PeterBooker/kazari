package artifact

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/audit"
)

// ImportRequest creates an artifact from a snapshot of a web page. Files are
// added to the snapshot's and win on a path clash.
type ImportRequest struct {
	URL         string
	Slug        string
	Name        string // defaults to the page title
	Description string // defaults to the page's meta description
	Visibility  Visibility
	Metadata    map[string]string
	Label       string
	KeepScripts bool
	Files       []FileInput
}

// ImportReport says what an import fetched, left out and found.
type ImportReport struct {
	Source         string
	Fetched        int
	Skipped        []SkippedFetch
	ScriptsRemoved int
	Notes          []string
	Outline        []Landmark
}

// SkippedFetch is a resource an import left remote, with the reason.
type SkippedFetch struct {
	URL    string
	Reason string
}

// Landmark is a section of an imported page: the line its opening tag is
// on, that tag as text, and the first heading inside it.
type Landmark struct {
	Line    int
	Tag     string
	Heading string
}

// Snapshot is a page as a PageSource took it; Close releases its files.
type Snapshot struct {
	Title       string
	Description string
	Files       []FileInput
	Report      ImportReport
	Close       func() error
}

// PageSource takes snapshots of web pages for Import.
type PageSource interface {
	Take(ctx context.Context, url string, keepScripts bool) (*Snapshot, error)
}

// Import takes a snapshot of req.URL and publishes it as a new artifact.
func (s *Service) Import(ctx context.Context, actor Actor, req ImportRequest) (*Artifact, *Version, *ImportReport, error) {
	if s.opts.Pages == nil {
		return nil, nil, nil, Errorf(ErrForbidden, "fetching URLs is disabled on this instance")
	}
	if actor.Handle == "" {
		return nil, nil, nil, Errorf(ErrForbidden, "an authenticated actor is required")
	}
	// The fetch is the expensive part, so the cheap refusals come first.
	if err := ValidateSlug(req.Slug, s.opts.ReservedSlugs); err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.store.Get(ctx, req.Slug); err == nil {
		return nil, nil, nil, Errorf(ErrSlugTaken, "slug %q is already taken", req.Slug)
	} else if CodeOf(err) != ErrNotFound.Code {
		return nil, nil, nil, err
	}
	snap, err := s.opts.Pages.Take(ctx, req.URL, req.KeepScripts)
	if err != nil {
		return nil, nil, nil, err
	}
	if snap.Close != nil {
		defer snap.Close()
	}
	name, desc := req.Name, req.Description
	if name == "" {
		name = cleanText(snap.Title, NameMaxBytes)
	}
	if name == "" {
		name = req.Slug
	}
	if desc == "" {
		desc = cleanText(snap.Description, DescriptionMaxBytes)
	}
	a, v, err := s.Create(ctx, actor, CreateRequest{
		Slug:        req.Slug,
		Name:        name,
		Description: desc,
		Visibility:  req.Visibility,
		Metadata:    req.Metadata,
		Label:       req.Label,
		Files:       mergeFiles(req.Files, snap.Files),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	details := versionDetails(v)
	details["source"] = snap.Report.Source
	s.record(ctx, actor, audit.ArtifactImport, a.Slug, details)
	rep := snap.Report
	return a, v, &rep, nil
}

// mergeFiles returns primary plus the entries of secondary whose path
// primary does not name.
func mergeFiles(primary, secondary []FileInput) []FileInput {
	taken := make(map[string]bool, len(primary))
	for _, f := range primary {
		taken[cleanPath(f.Path)] = true
	}
	out := append([]FileInput(nil), primary...)
	for _, f := range secondary {
		if !taken[cleanPath(f.Path)] {
			out = append(out, f)
		}
	}
	return out
}

func cleanPath(p string) string {
	if c, err := ValidatePath(p); err == nil {
		return c
	}
	return p
}

// cleanText makes page text usable as a name: one line, no control
// characters, at most max bytes on a rune boundary.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}
