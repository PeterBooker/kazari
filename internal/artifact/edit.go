package artifact

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxEdits caps the edits in one request, MaxReplacements the places one
// ReplaceAll edit may change.
const (
	MaxEdits        = 100
	MaxReplacements = 10000
)

// EditOutcome.Match values. Text edits try an exact match, then whole lines
// ignoring trailing whitespace, then ignoring indentation (re-indenting the
// new text).
const (
	MatchExact              = "exact"
	MatchTrailingWhitespace = "trailing_whitespace"
	MatchIndentation        = "indentation"
	MatchAnchor             = "anchor"
)

// ExcerptContext is the lines an edit's excerpt shows on each side of its new
// text; a new text of more than ExcerptFullLines lines shows only its first
// and last lines in full. maxFuzzyLines caps the old text that
// whitespace-tolerant and closest matching try, bounding their memory.
const (
	ExcerptContext   = 2
	ExcerptFullLines = 4
)

const (
	excerptMax       = 24
	excerptLineBytes = 160
	similarityBytes  = 512
	maxFuzzyLines    = 2000
)

// Edit is one change to a text file: NewText replaces OldText, which must
// match once unless ReplaceAll, or the lines Start to End inclusive (End
// defaults to Start), or goes in as new lines After or Before an anchored
// line. Anchors ("12:3fa0") come from AnchorLines; one request uses text
// edits or anchor edits, not both.
type Edit struct {
	OldText    string
	NewText    string
	ReplaceAll bool
	Start      string
	End        string
	After      string
	Before     string
}

// EditOutcome reports how one edit applied.
type EditOutcome struct {
	Match        string
	Replacements int
	// StartLine and EndLine are the 1-based lines of the first replacement's
	// new text in the edited file; EndLine is StartLine-1 when it is empty.
	StartLine int
	EndLine   int
	// Excerpt is those lines and a few around them, anchored as by
	// AnchorLines (long lines clipped) so an anchor edit needs no fresh read.
	// The inner lines of a long new text the caller sent verbatim are bare
	// anchors. Edits whose excerpts would overlap share the first one's:
	// ExcerptOf is then that edit's 1-based number and Excerpt is empty.
	Excerpt   string
	ExcerptOf int
}

// EditRequest edits one text file of the base version into a new version.
type EditRequest struct {
	Slug            string
	Path            string
	Edits           []Edit
	Label           string
	ExpectedVersion int  // compared with LatestVersion; 0 = no check
	BaseVersion     int  // the version edited, as in UpdateRequest
	KeepStale       bool // a new version normally clears stale_at/expires_at
	Stage           bool // as in UpdateRequest
}

const editAttempts = 3

// Edit applies req.Edits to a file of the base version and publishes the
// result as a new version, returning an outcome per edit.
func (s *Service) Edit(ctx context.Context, actor Actor, req EditRequest) (_ *Artifact, v *Version, out []EditOutcome, err error) {
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
	for attempt := 1; ; attempt++ {
		a, v, out, err := s.editOnce(ctx, actor, req, clean)
		if CodeOf(err) == ErrVersionConflict.Code && req.ExpectedVersion == 0 && attempt < editAttempts {
			s.m.editRetries.Add(ctx, 1)
			continue // a publish raced the read: re-apply to the newer content
		}
		return a, v, out, err
	}
}

func (s *Service) editOnce(ctx context.Context, actor Actor, req EditRequest, clean string) (*Artifact, *Version, []EditOutcome, error) {
	a, err := s.store.Get(ctx, req.Slug)
	if err != nil {
		return nil, nil, nil, err
	}
	involved, err := s.involved(ctx, actor, req.Slug)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := s.checkWritable(actor, a, involved, false); err != nil {
		return nil, nil, nil, err
	}
	if req.ExpectedVersion != 0 && req.ExpectedVersion != a.LatestVersion {
		return nil, nil, nil, s.editConflict(ctx, a, req, clean)
	}
	base, err := baseVersion(a, req.BaseVersion, req.Stage)
	if err != nil {
		return nil, nil, nil, err
	}
	data, f, err := s.readText(ctx, a.Slug, base, clean)
	if err != nil {
		return nil, nil, nil, err
	}
	edited, out, err := ApplyEdits(clean, data, req.Edits, s.opts.MaxFileBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	if edited == data {
		return nil, nil, nil, Errorf(ErrInvalidArgument, "the edits leave %q unchanged", clean)
	}
	label := req.Label
	if label == "" {
		label = editLabel(clean, len(req.Edits))
	}
	got, v, err := s.update(ctx, actor, UpdateRequest{
		Slug:            a.Slug,
		Label:           label,
		ExpectedVersion: a.LatestVersion,
		BaseVersion:     base, // the version the edits were applied to
		KeepStale:       req.KeepStale,
		Stage:           req.Stage,
		Files:           []FileInput{{Path: clean, Content: strings.NewReader(edited), ContentType: f.ContentType}},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return got, v, out, nil
}

// editConflict is the version_conflict of an edit sent with an old
// expected_version. It says whether the edits still apply as sent to the
// version they would now change, so a caller whose edits do not touch what
// changed resends them without a new read.
func (s *Service) editConflict(ctx context.Context, a *Artifact, req EditRequest, p string) error {
	msg := fmt.Sprintf("latest version is %d, expected %d", a.LatestVersion, req.ExpectedVersion)
	number := req.BaseVersion
	if number <= 0 || number > a.LatestVersion {
		number = a.LatestVersion
	}
	data, _, err := s.readText(ctx, a.Slug, number, p)
	if err == nil {
		var edited string
		edited, _, err = ApplyEdits(p, data, req.Edits, s.opts.MaxFileBytes)
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
		return "", Errorf(ErrTooLarge, "%q is over the %s file limit", f.Path, FormatBytes(s.opts.MaxFileBytes))
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", Errorf(ErrInvalidArgument, "%q is not a UTF-8 text file, so it cannot be edited; replace the whole file with update_artifact instead", f.Path)
	}
	return string(data), nil
}

// applyFileEdits returns inputs with each entry that carries Edits turned
// into the edited content of that file of the base version, and how the
// edits applied.
func (s *Service) applyFileEdits(ctx context.Context, base map[string]File, number int, inputs []FileInput) ([]FileInput, []FileEdits, error) {
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
		if !ok {
			return nil, nil, Errorf(ErrFileNotFound, "no file %q in version %d to edit; to add it, send its whole content", p, number)
		}
		data, err := s.fileText(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		text, outs, err := ApplyEdits(p, data, in.Edits, s.opts.MaxFileBytes)
		if err != nil {
			return nil, nil, err
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

// ApplyEdits edits content; name is only for messages. Text edits apply in
// order, each to the previous result; anchor edits all refer to content as
// given, must not overlap and apply together. A result over maxBytes is
// ErrTooLarge; 0 means no limit.
func ApplyEdits(name, content string, edits []Edit, maxBytes int64) (string, []EditOutcome, error) {
	if len(edits) == 0 {
		return "", nil, Errorf(ErrInvalidArgument, "at least one edit is required")
	}
	if len(edits) > MaxEdits {
		return "", nil, Errorf(ErrInvalidArgument, "%d edits in one call; at most %d", len(edits), MaxEdits)
	}
	anchors := 0
	for i, e := range edits {
		kinds := 0
		for _, f := range []string{e.OldText, e.Start, e.After, e.Before} {
			if f != "" {
				kinds++
			}
		}
		switch {
		case kinds > 1:
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: give one of old_text, start, after or before", i+1)
		case kinds == 0 && e.End != "":
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: end needs a start anchor", i+1)
		case kinds == 0:
			return "", nil, Errorf(ErrInvalidArgument,
				"edit %d: give old_text (the exact text to replace), a start anchor from a read with anchors, or after or before to insert lines", i+1)
		case e.End != "" && e.Start == "" && e.OldText != "":
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: end applies to anchor edits; use start and end, or old_text alone", i+1)
		case e.End != "" && e.Start == "":
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: end applies only with start; an insert takes after or before alone", i+1)
		case (e.After != "" || e.Before != "") && e.NewText == "":
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: an insert needs new_text", i+1)
		case e.OldText == "":
			anchors++
		}
	}
	if anchors != 0 && anchors != len(edits) {
		return "", nil, Errorf(ErrInvalidArgument,
			"mixing old_text and anchor edits in one call is not supported; send them in separate calls")
	}
	ed := &editor{name: name, text: content, max: maxBytes}
	var out []EditOutcome
	var err error
	if anchors > 0 {
		out, err = ed.applyAnchors(edits)
	} else {
		out, err = ed.applyTexts(edits)
	}
	if err != nil {
		return "", nil, err
	}
	offs := make([]int, 0, 2*len(out))
	for _, r := range ed.ranges {
		offs = append(offs, r[0], max(r[1]-1, r[0]))
	}
	lines := linesAt(ed.text, offs)
	for i := range out {
		r := ed.ranges[i]
		out[i].StartLine = lines[2*i]
		out[i].EndLine = out[i].StartLine - 1
		if r[1] > r[0] {
			out[i].EndLine = lines[2*i+1]
		}
	}
	excerpts(ed.text, out)
	return ed.text, out, nil
}

// editor keeps, per applied edit, the byte range of its first new text,
// moved as later edits change the text.
type editor struct {
	name   string
	text   string
	max    int64 // 0 = no limit
	ranges [][2]int
}

func (ed *editor) checkSize(size int64) error {
	if ed.max > 0 && size > ed.max {
		return Errorf(ErrTooLarge, "the edits would make %s %s, over the %s file limit", ed.name, FormatBytes(size), FormatBytes(ed.max))
	}
	return nil
}

// apply replaces ms, which must be sorted and not overlap, moves ed.ranges
// with the text and returns where each new text landed.
func (ed *editor) apply(ms []match) ([][2]int, error) {
	size := int64(len(ed.text))
	for _, m := range ms {
		size += int64(len(m.repl)) - int64(m.end-m.start)
	}
	if err := ed.checkSize(size); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.Grow(int(size))
	moved := make([][4]int, len(ms)) // old start, old end, new start, new end
	prev := 0
	for i, m := range ms {
		b.WriteString(ed.text[prev:m.start])
		start := b.Len()
		b.WriteString(m.repl)
		moved[i] = [4]int{m.start, m.end, start, b.Len()}
		prev = m.end
	}
	b.WriteString(ed.text[prev:])
	// An offset inside a replaced span moves to the start (or, for an end,
	// the end) of its new text, so a range overlapping a change grows to
	// cover it.
	move := func(off int, end bool) int {
		k := sort.Search(len(moved), func(k int) bool { return moved[k][1] > off })
		if k < len(moved) && moved[k][0] < off {
			if end {
				return moved[k][3]
			}
			return moved[k][2]
		}
		if k == 0 {
			return off
		}
		return off + moved[k-1][3] - moved[k-1][1]
	}
	for i, r := range ed.ranges {
		ed.ranges[i] = [2]int{move(r[0], false), move(r[1], true)}
	}
	ed.text = b.String()
	out := make([][2]int, len(moved))
	for i, m := range moved {
		out[i] = [2]int{m[2], m[3]}
	}
	return out, nil
}

func (ed *editor) applyTexts(edits []Edit) ([]EditOutcome, error) {
	out := make([]EditOutcome, 0, len(edits))
	for i, e := range edits {
		if e.OldText == e.NewText {
			return nil, Errorf(ErrInvalidArgument, "edit %d: old_text and new_text are the same", i+1)
		}
		o, err := ed.applyText(i+1, e)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

type match struct {
	start, end int
	repl       string
	// A line match has no repl until it is known to be wanted and to fit:
	// buildRepls fills it from the lines up to linesEnd, and dropBreak says
	// the new text's last break goes because the file ended before the old
	// text's.
	linesEnd  int
	dropBreak bool
}

func (ed *editor) applyText(n int, e Edit) (EditOutcome, error) {
	kind := MatchExact
	matches := exactMatches(ed.text, e.OldText, e.NewText)
	fuzzy := lineCount(e.OldText) <= maxFuzzyLines
	if len(matches) == 0 && fuzzy {
		kind = MatchTrailingWhitespace
		matches = lineMatches(ed.text, e, trimTrailing)
		if len(matches) == 0 {
			kind = MatchIndentation
			matches = lineMatches(ed.text, e, strings.TrimSpace)
		}
	}
	count := strconv.Itoa(len(matches))
	if len(matches) > MaxReplacements {
		count = "more than " + strconv.Itoa(MaxReplacements)
	}
	switch {
	case len(matches) == 0 && !fuzzy:
		return EditOutcome{}, Errorf(ErrEditNotFound,
			"edit %d: old_text was not found in %s; it has more than %d lines, too many to match ignoring whitespace, so copy it exactly or use anchor edits",
			n, ed.name, maxFuzzyLines)
	case len(matches) == 0:
		return EditOutcome{}, notFound(ed.name, n, ed.text, e.OldText)
	case len(matches) > 1 && !e.ReplaceAll:
		lines := make([]string, 0, 5)
		for _, m := range matches[:min(len(matches), 5)] {
			lines = append(lines, strconv.Itoa(lineAt(ed.text, m.start)))
		}
		how := ""
		if kind != MatchExact {
			how = " when whitespace is ignored"
		}
		return EditOutcome{}, Errorf(ErrEditAmbiguous,
			"edit %d: old_text matches %s places in %s%s (lines %s); add surrounding lines so it matches once, or set replace_all",
			n, count, ed.name, how, strings.Join(lines, ", "))
	case len(matches) > MaxReplacements:
		return EditOutcome{}, Errorf(ErrInvalidArgument,
			"edit %d: replace_all would change %s places in %s; at most %d per edit, so make old_text more specific",
			n, count, ed.name, MaxReplacements)
	}
	if kind != MatchExact {
		if err := ed.buildRepls(matches, e, kind == MatchIndentation); err != nil {
			return EditOutcome{}, err
		}
	}
	moved, err := ed.apply(matches)
	if err != nil {
		return EditOutcome{}, err
	}
	ed.ranges = append(ed.ranges, moved[0])
	return EditOutcome{Match: kind, Replacements: len(matches)}, nil
}

// exactMatches stops at MaxReplacements+1, enough to detect the cap.
func exactMatches(text, old, repl string) []match {
	var out []match
	for off := 0; len(out) <= MaxReplacements; {
		i := strings.Index(text[off:], old)
		if i < 0 {
			break
		}
		start := off + i
		out = append(out, match{start: start, end: start + len(old), repl: repl})
		off = start + len(old)
	}
	return out
}

// buildRepls fits the new text to each line match, which copies it, so it
// runs only for an edit that passed the count and ambiguity rules, and it
// works out the exact size of the result before building anything: a copy
// is as long as the file's indentation times the new text's lines, which a
// small request can make far larger than the file limit.
func (ed *editor) buildRepls(ms []match, e Edit, reindent bool) error {
	oldLines := splitLines(e.OldText)
	whole := newLineRepl(e.NewText, oldLines, reindent)
	cut := whole
	if ms[len(ms)-1].dropBreak { // only the file's end can drop it
		cut = newLineRepl(strings.TrimSuffix(e.NewText, "\n"), oldLines, reindent)
	}
	repl := func(m match) *lineRepl {
		if m.dropBreak {
			return cut
		}
		return whole
	}
	size := int64(len(ed.text))
	for _, m := range ms {
		first, later := whole.indents(ed.text[m.start:m.linesEnd])
		size += repl(m).size(first, later, endsCRLF(ed.text, m.start)) - int64(m.end-m.start)
	}
	if err := ed.checkSize(size); err != nil {
		return err
	}
	for i := range ms {
		m := &ms[i]
		first, later := whole.indents(ed.text[m.start:m.linesEnd])
		m.repl = repl(*m).build(first, later, endsCRLF(ed.text, m.start))
	}
	return nil
}

// lineMatches finds runs of whole lines equal to e.OldText's after norm. It
// is a Knuth-Morris-Pratt search over lines, so a repetitive file stays
// linear, and stops after MaxReplacements+1 runs.
func lineMatches(text string, e Edit, norm func(string) string) []match {
	oldLines := splitLines(e.OldText)
	if len(oldLines) == 0 {
		return nil
	}
	want := make([]string, len(oldLines))
	blank := true
	for i, l := range oldLines {
		want[i] = norm(l)
		blank = blank && want[i] == ""
	}
	if blank {
		return nil // whitespace only: would match any blank run
	}
	// fail is the KMP failure function of want.
	fail := make([]int, len(want))
	for i, k := 1, 0; i < len(want); i++ {
		for k > 0 && want[i] != want[k] {
			k = fail[k-1]
		}
		if want[i] == want[k] {
			k++
		}
		fail[i] = k
	}
	n := len(want)
	starts := make([]int, n) // starts of the last n lines, by line index mod n
	withNL := strings.HasSuffix(e.OldText, "\n")
	var out []match
	k := 0
	eachLine(text, func(i, start int, line string) bool {
		starts[i%n] = start
		l := norm(line)
		for k > 0 && l != want[k] {
			k = fail[k-1]
		}
		if l == want[k] {
			k++
		}
		if k < n {
			return true
		}
		k = 0 // matches do not overlap
		// Keep the last line's break unless the old text included one.
		m := match{start: starts[(i-n+1)%n], end: start + len(strings.TrimSuffix(line, "\r")), linesEnd: start + len(line)}
		if withNL {
			if start+len(line) < len(text) {
				m.end = start + len(line) + 1
			} else {
				m.dropBreak = true
			}
		}
		out = append(out, m)
		return len(out) <= MaxReplacements
	})
	return out
}

func trimTrailing(s string) string { return strings.TrimRight(s, " \t\r") }

func leading(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// lineRepl is one edit's new text, readied once for all its line matches, so
// that size is constant and build costs what it returns, however much of the
// new text was indentation the old text shares.
//
// The first line takes the file's first indentation; later lines shift by the
// difference at the first non-blank later line (else the first line's), since
// text copied from a page often starts after its first indentation. A line
// only shifts if it is not blank and starts with the old indentation.
type lineRepl struct {
	text   string // the new text less the old indentation of the lines that shift
	shifts []bool // by line of text; nil if not re-indenting
	first  int    // 1 if the first line shifts
	later  int    // how many other lines shift
	guide  int    // the line of the old text that sets the later lines' shift
	breaks int    // line feeds in text
	crlf   bool   // text has a CRLF of its own, so fitBreaks leaves it alone
}

func newLineRepl(newText string, oldLines []string, reindent bool) *lineRepl {
	r := &lineRepl{text: newText, breaks: strings.Count(newText, "\n"), crlf: strings.Contains(newText, "\r\n")}
	if !reindent {
		return r
	}
	from0 := leading(oldLines[0])
	from := from0
	for j := 1; j < len(oldLines); j++ {
		if strings.TrimSpace(oldLines[j]) != "" {
			from, r.guide = leading(oldLines[j]), j
			break
		}
	}
	r.shifts = make([]bool, r.breaks+1)
	var b strings.Builder
	b.Grow(len(newText))
	for i := 0; ; i++ {
		l, rest, more := strings.Cut(newText, "\n")
		f := from
		if i == 0 {
			f = from0
		}
		if strings.HasPrefix(l, f) && strings.TrimSpace(l[len(f):]) != "" {
			l = l[len(f):]
			r.shifts[i] = true
			if i == 0 {
				r.first = 1
			} else {
				r.later++
			}
		}
		b.WriteString(l)
		if !more {
			break
		}
		b.WriteByte('\n')
		newText = rest
	}
	r.text = b.String()
	return r
}

// indents returns the indentation the first line and the later lines take
// from matched, the file's lines of one match.
func (r *lineRepl) indents(matched string) (first, later string) {
	first = leading(matched)
	for range r.guide {
		matched = matched[strings.IndexByte(matched, '\n')+1:]
	}
	return first, leading(matched)
}

func (r *lineRepl) added(first, later string) int64 {
	return int64(r.first)*int64(len(first)) + int64(r.later)*int64(len(later))
}

// size is the length of what build returns.
func (r *lineRepl) size(first, later string, crlf bool) int64 {
	n := int64(len(r.text)) + r.added(first, later)
	if crlf && !r.crlf {
		n += int64(r.breaks)
	}
	return n
}

func (r *lineRepl) build(first, later string, crlf bool) string {
	s := r.text
	if added := r.added(first, later); added > 0 {
		var b strings.Builder
		b.Grow(len(s) + int(added))
		for i := 0; ; i++ {
			l, rest, more := strings.Cut(s, "\n")
			if r.shifts[i] {
				if i == 0 {
					b.WriteString(first)
				} else {
					b.WriteString(later)
				}
			}
			b.WriteString(l)
			if !more {
				break
			}
			b.WriteByte('\n')
			s = rest
		}
		s = b.String()
	}
	return fitBreaks(s, crlf)
}

func endsCRLF(text string, off int) bool {
	k := strings.IndexByte(text[off:], '\n')
	return k > 0 && text[off+k-1] == '\r'
}

func fitBreaks(s string, crlf bool) string {
	if !crlf || strings.Contains(s, "\r\n") {
		return s
	}
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func notFound(name string, n int, text, old string) error {
	c := closest(text, old)
	if c.score < 0.5 {
		return Errorf(ErrEditNotFound,
			"edit %d: old_text was not found in %s and nothing similar was found; read the file again, it may have changed",
			n, name)
	}
	first, last := c.start+1, c.start+len(c.lines)
	return Errorf(ErrEditNotFound,
		"edit %d: old_text was not found in %s. The closest text is lines %d-%d (%d%% similar):\n%s\n"+
			"Copy old_text exactly from these lines, or replace them by anchor: start %q, end %q.",
		n, name, first, last, int(c.score*100), anchored(c.lines, first, max(0, len(c.lines)-excerptMax)),
		Anchor(first, c.lines[0]), Anchor(last, c.lines[len(c.lines)-1]))
}

// closestCandidates is how many windows closest scores in full for each of
// the old text's first and last lines.
const closestCandidates = 20

type near struct {
	start int // 0-based line
	lines []string
	score float64 // 0 to 1
}

// closest finds the run of lines most similar to old. Scoring every window
// costs lines(text) x lines(old), so one pass picks the windows whose first
// or last line matches best and a second scores only those.
func closest(text, old string) near {
	want := splitLines(old)
	for i := range want {
		want[i] = similarityKey(want[i])
	}
	total := lineCount(text)
	n := min(len(want), total)
	if n == 0 {
		return near{}
	}
	want = want[:n]
	first := slices.IndexFunc(want, func(s string) bool { return s != "" })
	if first < 0 {
		return near{}
	}
	last := n - 1
	for want[last] == "" {
		last--
	}
	windows := total - n + 1
	firstGrams, lastGrams := bigrams(nil, want[first]), bigrams(nil, want[last])
	best1, best2 := &top{}, &top{}
	var g []uint16
	eachLine(text, func(i, _ int, line string) bool {
		g = bigrams(g[:0], similarityKey(line))
		if w := i - first; w >= 0 && w < windows {
			best1.add(w, dice(firstGrams, g))
		}
		if w := i - last; w >= 0 && w < windows {
			best2.add(w, dice(lastGrams, g))
		}
		return true
	})
	cands := append(best1.starts(), best2.starts()...)
	slices.Sort(cands)
	cands = slices.Compact(cands)
	wantGrams := make([][]uint16, n)
	for j, w := range want {
		wantGrams[j] = bigrams(nil, w)
	}
	sums := make([]float64, len(cands))
	lines := make([][]string, len(cands))
	lo := 0 // cands[lo:] are the windows that have not ended yet
	eachLine(text, func(i, _ int, line string) bool {
		for lo < len(cands) && cands[lo]+n <= i {
			lo++
		}
		if lo == len(cands) {
			return false
		}
		ready := false
		for c := lo; c < len(cands) && cands[c] <= i; c++ {
			if !ready {
				g, ready = bigrams(g[:0], similarityKey(line)), true
			}
			sums[c] += dice(wantGrams[i-cands[c]], g)
			lines[c] = append(lines[c], line)
		}
		return true
	})
	best := near{score: -1}
	for c, w := range cands {
		if s := sums[c] / float64(n); s > best.score {
			best = near{start: w, lines: lines[c], score: s}
		}
	}
	return best
}

func similarityKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > similarityBytes {
		s = s[:similarityBytes]
	}
	return s
}

// top keeps the window starts with the closestCandidates highest scores.
type top struct {
	ws     []int
	scores []float64
}

func (t *top) add(w int, score float64) {
	if len(t.ws) == closestCandidates && score <= t.scores[len(t.scores)-1] {
		return
	}
	i := sort.Search(len(t.scores), func(i int) bool { return t.scores[i] < score })
	t.ws = slices.Insert(t.ws, i, w)
	t.scores = slices.Insert(t.scores, i, score)
	if len(t.ws) > closestCandidates {
		t.ws, t.scores = t.ws[:closestCandidates], t.scores[:closestCandidates]
	}
}

func (t *top) starts() []int { return t.ws }

// bigrams appends the byte pairs of s to dst, sorted; a 1-byte s is its own
// pair.
func bigrams(dst []uint16, s string) []uint16 {
	if len(s) < 2 {
		if s == "" {
			return dst
		}
		return append(dst, uint16(s[0]))
	}
	start := len(dst)
	for i := 0; i+1 < len(s); i++ {
		dst = append(dst, uint16(s[i])<<8|uint16(s[i+1]))
	}
	slices.Sort(dst[start:])
	return dst
}

// dice is the Sørensen-Dice coefficient of two sorted bigram multisets.
func dice(a, b []uint16) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	common := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			common++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return 2 * float64(common) / float64(len(a)+len(b))
}

type anchorEdit struct {
	idx        int // position in the request
	from, to   int // 1-based lines
	insert     string
	start, end int // bytes
	repl       string
	lead       int // bytes of repl before the new text: a break after a last line that had none
}

func (a anchorEdit) lines() string {
	if a.insert != "" {
		return fmt.Sprintf("%s line %d", a.insert, a.from)
	}
	return fmt.Sprintf("lines %d-%d", a.from, a.to)
}

type lineSpan struct {
	start int
	text  string
	next  int // start of the next line
}

func (ed *editor) applyAnchors(edits []Edit) ([]EditOutcome, error) {
	type parsed struct {
		from, to int
		insert   string
	}
	ps := make([]parsed, len(edits))
	hashes := map[int]string{}
	for i, e := range edits {
		first, insert := e.Start, ""
		switch {
		case e.After != "":
			first, insert = e.After, "after"
		case e.Before != "":
			first, insert = e.Before, "before"
		}
		from, h, err := ParseAnchor(first)
		if err != nil {
			return nil, Errorf(ErrInvalidArgument, "edit %d: %v", i+1, err)
		}
		hashes[from] = h
		to := from
		if e.End != "" {
			if to, h, err = ParseAnchor(e.End); err != nil {
				return nil, Errorf(ErrInvalidArgument, "edit %d: %v", i+1, err)
			}
			hashes[to] = h
		}
		if to < from {
			return nil, Errorf(ErrInvalidArgument, "edit %d: end (line %d) is before start (line %d)", i+1, to, from)
		}
		ps[i] = parsed{from, to, insert}
	}
	spans := map[int]lineSpan{}
	total := 0
	eachLine(ed.text, func(i, start int, line string) bool {
		total = i + 1
		if _, ok := hashes[i+1]; ok {
			spans[i+1] = lineSpan{start: start, text: line, next: min(start+len(line)+1, len(ed.text))}
		}
		return true
	})
	resolved := make([]anchorEdit, 0, len(edits))
	for i, e := range edits {
		for _, a := range []string{e.Start, e.End, e.After, e.Before} {
			if a == "" {
				continue
			}
			if err := ed.checkAnchor(i+1, a, spans, total); err != nil {
				return nil, err
			}
		}
		from, to := spans[ps[i].from], spans[ps[i].to]
		eol := ed.text[to.start+len(to.text) : to.next] // "\n", or "" for a last line without one
		crlf := eol == "\n" && strings.HasSuffix(to.text, "\r")
		ae := anchorEdit{idx: i, from: ps[i].from, to: ps[i].to, insert: ps[i].insert, start: from.start, end: to.next}
		repl := fitBreaks(e.NewText, crlf)
		brk := fitBreaks("\n", crlf)
		switch {
		case ae.insert == "before":
			ae.end = ae.start
		case ae.insert == "after" && eol == "":
			// The new lines follow a last line that had no break, so they
			// bring one before them and keep the file's missing final break.
			crlf = from.start >= 2 && ed.text[from.start-2] == '\r'
			brk = fitBreaks("\n", crlf)
			repl = brk + fitBreaks(e.NewText, crlf)
			ae.start, ae.end, ae.lead = to.next, to.next, len(brk)
			ae.repl = repl
			resolved = append(resolved, ae)
			continue
		case ae.insert == "after":
			ae.start = ae.end
		}
		if repl != "" && (eol != "" || ae.insert != "") && !strings.HasSuffix(repl, "\n") {
			repl += brk // the lines around the new text are whole lines
		}
		ae.repl = repl
		resolved = append(resolved, ae)
	}
	byPos := slices.Clone(resolved)
	// An insert sorts before a replacement that starts where it does, and two
	// inserts at one place keep the order of the request.
	slices.SortStableFunc(byPos, func(a, b anchorEdit) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end))
	})
	for k := 1; k < len(byPos); k++ {
		if p, c := byPos[k-1], byPos[k]; c.start < p.end {
			return nil, Errorf(ErrInvalidArgument, "edits %d and %d overlap (%s and %s)",
				min(p.idx, c.idx)+1, max(p.idx, c.idx)+1, p.lines(), c.lines())
		}
	}
	ms := make([]match, len(byPos))
	for k, a := range byPos {
		ms[k] = match{start: a.start, end: a.end, repl: a.repl}
	}
	moved, err := ed.apply(ms)
	if err != nil {
		return nil, err
	}
	ed.ranges = make([][2]int, len(resolved))
	for k, a := range byPos {
		ed.ranges[a.idx] = [2]int{moved[k][0] + a.lead, moved[k][1]}
	}
	out := make([]EditOutcome, len(edits))
	for i := range out {
		out[i] = EditOutcome{Match: MatchAnchor, Replacements: 1}
	}
	return out, nil
}

// checkAnchor, on a mismatch, looks within 50 lines for where the line moved.
func (ed *editor) checkAnchor(n int, anchor string, spans map[int]lineSpan, total int) error {
	line, hash, _ := ParseAnchor(anchor) // parsed before
	if line > total {
		return Errorf(ErrAnchorMismatch, "edit %d: anchor %s is past the end of %s, which has %d lines; read it again with anchors",
			n, anchor, ed.name, total)
	}
	got := spans[line].text
	if lineHash(got) == hash {
		return nil
	}
	msg := fmt.Sprintf("edit %d: anchor %s does not match %s: line %d is now %s|%s", n, strings.TrimSpace(anchor), ed.name,
		line, Anchor(line, got), clip(got))
	moved := 0
	eachLine(ed.text, func(i, _ int, l string) bool {
		if i+1 > line+50 {
			return false
		}
		if d := i + 1 - line; d >= -50 && d != 0 && lineHash(l) == hash && (moved == 0 || abs(d) < abs(moved-line)) {
			moved = i + 1
		}
		return true
	})
	if moved != 0 {
		msg += fmt.Sprintf("; the line it named may have moved to %d:%s", moved, hash)
	}
	return Errorf(ErrAnchorMismatch, "%s. The file changed since it was read: read it again with anchors", msg)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func clip(s string) string {
	const limit = 80
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// lineHash hashes a line without its line break.
func lineHash(line string) string {
	var buf [4]byte
	return string(appendHash(buf[:0], line))
}

const fnvOffset = uint32(2166136261)

// fnv1a continues a 32-bit FNV-1a sum over b, so a line can be hashed in parts.
func fnv1a[T string | []byte](v uint32, b T) uint32 {
	for i := 0; i < len(b); i++ {
		v ^= uint32(b[i])
		v *= 16777619
	}
	return v
}

// appendHash appends line's 32-bit FNV-1a, folded to 16 bits, in hex.
func appendHash(dst []byte, line string) []byte {
	return appendSum(dst, fnv1a(fnvOffset, line))
}

func appendSum(dst []byte, v uint32) []byte {
	v = (v>>16 ^ v) & 0xffff
	const digits = "0123456789abcdef"
	return append(dst, digits[v>>12], digits[v>>8&0xf], digits[v>>4&0xf], digits[v&0xf])
}

// Anchor is the anchor of line number n with content line: "12:3fa0".
func Anchor(n int, line string) string {
	return string(appendAnchor(nil, n, line))
}

func appendAnchor(dst []byte, n int, line string) []byte {
	return appendAnchorSum(dst, n, fnv1a(fnvOffset, line))
}

func appendAnchorSum(dst []byte, n int, sum uint32) []byte {
	dst = strconv.AppendInt(dst, int64(n), 10)
	dst = append(dst, ':')
	return appendSum(dst, sum)
}

// ParseAnchor splits "12:3fa0" into line number and hash, ignoring anything
// from a "|" on so a whole line copied from AnchorLines works.
func ParseAnchor(s string) (int, string, error) {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "|")
	num, hash, ok := strings.Cut(s, ":")
	hash = strings.ToLower(hash)
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n < 1 || len(hash) != 4 || strings.Trim(hash, "0123456789abcdef") != "" {
		return 0, "", fmt.Errorf("anchor %q is not of the form <line>:<hash>, e.g. 12:3fa0", s)
	}
	return n, hash, nil
}

// AnchorLines prefixes every line with its anchor and "|", e.g.
// "12:3fa0|<h1>Title</h1>"; a final line break adds no extra line.
func AnchorLines(content string) string {
	var b strings.Builder
	_ = WriteAnchored(&b, content) // a strings.Builder does not fail
	return b.String()
}

// WriteAnchored streams AnchorLines(content) to w.
func WriteAnchored(w io.Writer, content string) error {
	return StreamAnchored(w, strings.NewReader(content))
}

// ErrFileRead marks a StreamAnchored failure of the file, not of the writer.
var ErrFileRead = errors.New("artifact: read file")

// AnchorableText reports whether r holds UTF-8 text with no NUL, the only
// files with line anchors. It reads r to the end in a fixed buffer.
func AnchorableText(r io.Reader) (bool, error) {
	br := bufio.NewReader(r)
	for {
		c, size, err := br.ReadRune()
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if c == 0 || c == utf8.RuneError && size == 1 {
			return false, nil
		}
	}
}

// StreamAnchored writes what AnchorLines gives for the text in r, which starts
// at its first byte, holding only fixed buffers however long a line is: w may
// block for as long as its reader likes. A line that outgrows the buffer is
// hashed first, because its anchor comes before it, then read again from where
// it started.
func StreamAnchored(w io.Writer, r io.ReadSeeker) error {
	bw := bufio.NewWriter(w)
	br := bufio.NewReader(r)
	var prefix []byte
	var off int64
	for n := 1; ; n++ {
		sum, passed := fnvOffset, int64(0)
		chunk, err := br.ReadSlice('\n')
		for errors.Is(err, bufio.ErrBufferFull) {
			sum = fnv1a(sum, chunk)
			passed += int64(len(chunk))
			chunk, err = br.ReadSlice('\n')
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("%w: %w", ErrFileRead, err)
		}
		if passed == 0 && len(chunk) == 0 {
			return bw.Flush() // reports the first write error
		}
		sum = fnv1a(sum, bytes.TrimSuffix(chunk, []byte{'\n'}))
		prefix = append(appendAnchorSum(prefix[:0], n, sum), '|')
		if _, werr := bw.Write(prefix); werr != nil {
			return werr
		}
		size := passed + int64(len(chunk))
		if passed == 0 {
			if _, werr := bw.Write(chunk); werr != nil {
				return werr
			}
		} else if err := copyLineFrom(bw, br, r, off); err != nil {
			return err
		}
		off += size
		if err == io.EOF {
			return bw.Flush()
		}
	}
}

// copyLineFrom writes the line that starts at off, with its break, leaving br
// just after it.
func copyLineFrom(bw *bufio.Writer, br *bufio.Reader, r io.ReadSeeker, off int64) error {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("%w: %w", ErrFileRead, err)
	}
	br.Reset(r)
	for {
		chunk, err := br.ReadSlice('\n')
		if _, werr := bw.Write(chunk); werr != nil {
			return werr
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
		case err == nil, errors.Is(err, io.EOF):
			return nil
		default:
			return fmt.Errorf("%w: %w", ErrFileRead, err)
		}
	}
}

// lineShow is how an excerpt shows a line; a line two edits show differently
// takes the higher.
type lineShow uint8

const (
	showElided lineShow = iota + 1
	showBare
	showFull
)

type showSpan struct {
	from, to int // 1-based lines, inclusive
	show     lineShow
}

// excerptView is the lines one excerpt shows: from to to, in spans.
type excerptView struct {
	owner    int // the first edit, by request order, whose lines it shows
	from, to int
	spans    []showSpan
}

// excerpts fills each outcome's Excerpt from text, reading only the lines it
// shows. An edit's excerpt is its new text with ExcerptContext lines around
// it; inside a new text of more than ExcerptFullLines lines that the caller
// sent verbatim, only the first and last lines show their content, and the
// middle of one longer than excerptMax lines is left out. Excerpts that
// overlap or touch become one, given to the first of their edits.
func excerpts(text string, outs []EditOutcome) {
	if text == "" || len(outs) == 0 {
		return
	}
	total := lineCount(text)
	views := make([]excerptView, len(outs))
	for i, o := range outs {
		views[i] = excerptOf(i, o, total)
	}
	slices.SortStableFunc(views, func(a, b excerptView) int { return cmp.Compare(a.from, b.from) })
	merged := views[:1]
	for _, v := range views[1:] {
		last := &merged[len(merged)-1]
		if v.from > last.to+1 {
			merged = append(merged, v)
			continue
		}
		outs[max(v.owner, last.owner)].ExcerptOf = min(v.owner, last.owner) + 1
		last.owner = min(v.owner, last.owner)
		last.to = max(last.to, v.to)
		last.spans = append(last.spans, v.spans...)
	}
	for i := range outs {
		if outs[i].ExcerptOf != 0 {
			// A group can take over another's owner after it gave an edit
			// away, so follow the chain to the excerpt that stays.
			for outs[outs[i].ExcerptOf-1].ExcerptOf != 0 {
				outs[i].ExcerptOf = outs[outs[i].ExcerptOf-1].ExcerptOf
			}
		}
	}
	bufs := make([]strings.Builder, len(merged))
	shows := make([][]showSpan, len(merged))
	for g := range merged {
		shows[g] = flatten(merged[g])
	}
	g, k, elided := 0, 0, 0
	eachLine(text, func(i, _ int, line string) bool {
		n := i + 1
		for g < len(merged) && n > merged[g].to {
			g, k = g+1, 0
		}
		if g == len(merged) {
			return false
		}
		v := merged[g]
		if n < v.from {
			return true
		}
		for shows[g][k].to < n {
			k++
		}
		b := &bufs[g]
		show := shows[g][k].show
		if show == showElided {
			elided++
			return true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if elided > 0 {
			fmt.Fprintf(b, "... %d lines not shown ...\n", elided)
			elided = 0
		}
		b.WriteString(Anchor(n, line))
		if show == showFull {
			b.WriteByte('|')
			b.WriteString(clipLine(line))
		}
		return true
	})
	for g, v := range merged {
		outs[v.owner].Excerpt = bufs[g].String()
	}
}

// excerptOf is the view of one edit's excerpt in a file of total lines; its
// spans do not overlap.
func excerptOf(i int, o EditOutcome, total int) excerptView {
	first, last := o.StartLine, o.EndLine
	if last < first {
		// Nothing new: show around where the text was, which is past the
		// end when the edit removed the last lines.
		first = min(first, total)
		last = first
	}
	v := excerptView{owner: i, from: max(1, first-ExcerptContext), to: min(total, last+ExcerptContext)}
	n := last - first + 1
	inner := showFull
	if n > ExcerptFullLines && o.Match != MatchIndentation {
		inner = showBare
	}
	if o.EndLine < o.StartLine || n <= 2 || inner == showFull && n <= excerptMax {
		v.spans = []showSpan{{v.from, v.to, showFull}}
		return v
	}
	v.spans = []showSpan{{v.from, first, showFull}}
	if n > excerptMax {
		half := excerptMax / 2
		v.spans = append(v.spans,
			showSpan{first + 1, first + half - 1, inner},
			showSpan{first + half, last - half, showElided},
			showSpan{last - half + 1, last - 1, inner})
	} else {
		v.spans = append(v.spans, showSpan{first + 1, last - 1, inner})
	}
	v.spans = append(v.spans, showSpan{last, v.to, showFull})
	return v
}

// flatten turns the spans of a merged view, which overlap where its edits'
// excerpts did, into a sorted run covering v.from to v.to, each line shown
// in the highest way any span asks for.
func flatten(v excerptView) []showSpan {
	cuts := []int{v.from, v.to + 1}
	for _, s := range v.spans {
		cuts = append(cuts, s.from, s.to+1)
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)
	var out []showSpan
	for j := 0; j+1 < len(cuts); j++ {
		from, to := cuts[j], cuts[j+1]-1
		if from < v.from || to > v.to {
			continue
		}
		show := lineShow(0)
		for _, s := range v.spans {
			if s.from <= from && to <= s.to {
				show = max(show, s.show)
			}
		}
		if n := len(out); n > 0 && out[n-1].show == show {
			out[n-1].to = to
			continue
		}
		out = append(out, showSpan{from, to, show})
	}
	return out
}

// anchored renders lines numbered from first; skip > 0 elides that many
// lines in the middle.
func anchored(lines []string, first, skip int) string {
	var b strings.Builder
	cut := -1
	if skip > 0 {
		cut = (len(lines) - skip) / 2
	}
	for i := 0; i < len(lines); i++ {
		if i == cut {
			fmt.Fprintf(&b, "... %d lines not shown ...\n", skip)
			i += skip - 1
			continue
		}
		b.WriteString(Anchor(first+i, lines[i]))
		b.WriteByte('|')
		b.WriteString(clipLine(lines[i]))
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// clipLine notes the bytes it leaves out; the anchor still hashes the whole
// line.
func clipLine(s string) string {
	if len(s) <= excerptLineBytes {
		return s
	}
	cut := excerptLineBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s…[+%d bytes]", s[:cut], len(s)-cut)
}

// eachLine calls fn for each line (0-based, without its break) until fn
// returns false; a final line break starts no new line.
func eachLine(text string, fn func(i, start int, line string) bool) {
	for i, off := 0, 0; off < len(text); i++ {
		end, next := len(text), len(text)
		if k := strings.IndexByte(text[off:], '\n'); k >= 0 {
			end, next = off+k, off+k+1
		}
		if !fn(i, off, text[off:end]) {
			return
		}
		off = next
	}
}

// splitLines counts "a\nb\n" as two lines and "" as none.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// lineCount is len(splitLines(s)).
func lineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// lineAt is 1-based.
func lineAt(text string, off int) int {
	return strings.Count(text[:min(off, len(text))], "\n") + 1
}

// linesAt is lineAt for many offsets in one pass.
func linesAt(text string, offs []int) []int {
	order := make([]int, len(offs))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(x, y int) int { return cmp.Compare(offs[x], offs[y]) })
	out := make([]int, len(offs))
	line, pos := 1, 0
	for _, i := range order {
		o := min(offs[i], len(text))
		line += strings.Count(text[pos:o], "\n")
		pos = o
		out[i] = line
	}
	return out
}
