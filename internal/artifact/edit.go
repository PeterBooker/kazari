package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/bytesize"
	"github.com/PeterBooker/kazari/internal/textedit"
)

// EditRequest edits one text file of the base version into a new version.
type EditRequest struct {
	Slug            string
	Path            string
	Edits           []textedit.Edit
	Label           string
	ExpectedVersion int  // compared with LatestVersion; 0 = no check
	BaseVersion     int  // the version edited, as in UpdateRequest
	KeepStale       bool // a new version normally clears stale_at/expires_at
	Stage           bool // as in UpdateRequest
}

const editAttempts = 3

// Edit applies req.Edits to a file of the base version and publishes the
// result as a new version, returning an outcome per edit.
func (s *Service) Edit(ctx context.Context, actor Actor, req EditRequest) (_ *Artifact, v *Version, out []textedit.Outcome, err error) {
	ctx, span := s.span(ctx, "artifact.edit", req.Slug)
	defer func() {
		s.published(ctx, publishEdit, v, err)
		s.edited(ctx, out)
		versionAttrs(span, v)
		editAttrs(span, out)
		endSpan(span, err)
	}()
	if err := checkSlug(actor, req.Slug); err != nil {
		return nil, nil, nil, err
	}
	if err := ValidateLabel(req.Label); err != nil {
		return nil, nil, nil, err
	}
	clean, err := ValidatePath(req.Path)
	if err != nil {
		return nil, nil, nil, err
	}
	label := req.Label
	if label == "" {
		label = editLabel(clean, len(req.Edits))
	}
	ureq := UpdateRequest{
		Slug:            req.Slug,
		Label:           label,
		ExpectedVersion: req.ExpectedVersion,
		BaseVersion:     req.BaseVersion,
		KeepStale:       req.KeepStale,
		Stage:           req.Stage,
		Files:           []FileInput{{Path: clean, Edits: req.Edits}},
	}
	for attempt := 1; ; attempt++ {
		a, v, err := s.change(ctx, actor, ureq, true)
		if CodeOf(err) == ErrVersionConflict.Code {
			if req.ExpectedVersion != 0 {
				return nil, nil, nil, s.editConflict(ctx, req, clean, err)
			}
			if attempt < editAttempts {
				s.m.editRetries.Add(ctx, 1)
				continue // a publish raced the read: re-apply to the newer content
			}
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if len(v.Edited) > 0 {
			out = v.Edited[0].Outcomes
		}
		return a, v, out, nil
	}
}

// editConflict is the version_conflict of an edit sent with an old
// expected_version. It says whether the edits still apply as sent to the
// version they would now change, so a caller whose edits do not touch what
// changed resends them without a new read.
func (s *Service) editConflict(ctx context.Context, req EditRequest, p string, conflict error) error {
	a, err := s.store.Get(ctx, req.Slug)
	if err != nil {
		return conflict
	}
	msg := fmt.Sprintf("latest version is %d, expected %d", a.LatestVersion, req.ExpectedVersion)
	number := req.BaseVersion
	if number <= 0 || number > a.LatestVersion {
		number = a.LatestVersion
	}
	data, _, err := s.readText(ctx, a.Slug, number, p)
	if err == nil {
		var edited string
		edited, _, err = textedit.Apply(p, data, req.Edits, s.opts.MaxFileBytes)
		if err == nil && edited != data {
			return Errorf(ErrVersionConflict, "%s; every edit still applies to version %d as sent: resend the same edits "+
				"with expected_version %d", msg, number, a.LatestVersion)
		}
	}
	return Errorf(ErrVersionConflict, "%s; the edits no longer apply as sent to version %d: read %s again and redo them",
		msg, number, p)
}

func editLabel(p string, edits int) string {
	suffix := " (1 edit)"
	if edits != 1 {
		suffix = fmt.Sprintf(" (%d edits)", edits)
	}
	return "edit " + truncate(p, LabelMaxBytes-len("edit ")-len(suffix)) + suffix
}

func (s *Service) readText(ctx context.Context, slug string, number int, p string) (string, File, error) {
	v, err := s.store.GetVersion(ctx, slug, number)
	if err != nil {
		return "", File{}, err
	}
	i := slices.IndexFunc(v.Files, func(f File) bool { return f.Path == p })
	if i < 0 {
		return "", File{}, Errorf(ErrFileNotFound, "no file %q in version %d; to add it, send its whole content as a new file", p, v.Number)
	}
	f := v.Files[i]
	data, err := s.fileText(ctx, f)
	return data, f, err
}

// fileText reads f as UTF-8 text without NUL bytes, up to the file limit.
func (s *Service) fileText(ctx context.Context, f File) (string, error) {
	rc, err := s.blobs.Open(ctx, f.Hash)
	if err != nil {
		return "", fmt.Errorf("artifact: open %s: %w", f.Path, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, s.opts.MaxFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("artifact: read %s: %w", f.Path, err)
	}
	if int64(len(data)) > s.opts.MaxFileBytes {
		return "", Errorf(ErrTooLarge, "%q is over the %s file limit", f.Path, bytesize.Format(s.opts.MaxFileBytes))
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", Errorf(ErrInvalidArgument, "%q is not a UTF-8 text file, so it cannot be edited; replace the whole file with update_artifact instead", f.Path)
	}
	return string(data), nil
}

// applyFileEdits returns inputs with each entry that carries Edits turned
// into the edited content of that file of the base version, and how the
// edits applied. For an Edit, edits that leave the file as it was are
// refused.
func (s *Service) applyFileEdits(ctx context.Context, base map[string]File, number int, inputs []FileInput, edit bool) ([]FileInput, []FileEdits, error) {
	var out []FileInput
	var edited []FileEdits
	for i, in := range inputs {
		if len(in.Edits) == 0 {
			continue
		}
		if in.Delete || in.Content != nil {
			return nil, nil, Errorf(ErrInvalidArgument, "file %q has edits and also content or delete; give one", in.Path)
		}
		p, err := ValidatePath(in.Path)
		if err != nil {
			return nil, nil, err
		}
		f, ok := base[p]
		switch {
		case !ok && edit:
			return nil, nil, Errorf(ErrFileNotFound, "no file %q in version %d; to add it, send its whole content as a new file", p, number)
		case !ok:
			return nil, nil, Errorf(ErrFileNotFound, "no file %q in version %d to edit; to add it, send its whole content", p, number)
		}
		data, err := s.fileText(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		text, outs, err := textedit.Apply(p, data, in.Edits, s.opts.MaxFileBytes)
		if err != nil {
			return nil, nil, err
		}
		if edit && text == data {
			return nil, nil, Errorf(ErrInvalidArgument, "the edits leave %q unchanged", p)
		}
		if out == nil {
			out = slices.Clone(inputs)
		}
		ct := in.ContentType
		if ct == "" {
			ct = f.ContentType
		}
		out[i] = FileInput{Path: p, Content: strings.NewReader(text), ContentType: ct}
		edited = append(edited, FileEdits{Path: p, Outcomes: outs})
	}
	if out == nil {
		return inputs, nil, nil
	}
	return out, edited, nil
}
