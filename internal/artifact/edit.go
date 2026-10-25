package artifact

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxEdits is the largest number of edits one EditRequest may carry, and
// MaxReplacements the most places one edit may change with ReplaceAll.
const (
	MaxEdits        = 100
	MaxReplacements = 10000
)

// How an edit matched, as reported in EditOutcome.Match. Text edits try
// MatchExact first, then compare whole lines ignoring trailing whitespace,
// then ignoring indentation (re-indenting the new text to fit).
const (
	MatchExact              = "exact"
	MatchTrailingWhitespace = "trailing_whitespace"
	MatchIndentation        = "indentation"
	MatchAnchor             = "anchor"
)

// Excerpts show excerptContext unchanged lines around an edit and at most
// excerptMax lines of its new text, each clipped to excerptLineBytes.
// Similarity looks at the first similarityBytes of a line. Whitespace-
// tolerant and closest matches are tried for an old text of at most
// maxFuzzyLines lines, which bounds their memory.
const (
	excerptContext   = 2
	excerptMax       = 24
	excerptLineBytes = 160
	similarityBytes  = 512
	maxFuzzyLines    = 2000
)

// Edit is one change to a text file. A text edit sets OldText: it is
// replaced by NewText, and must match once unless ReplaceAll. An anchor
// edit sets Start (and optionally End, default Start): the lines from Start
// to End inclusive are replaced by NewText. Anchors ("12:3fa0") come from
// AnchorLines; one request uses one kind of edit.
type Edit struct {
	OldText    string
	NewText    string
	ReplaceAll bool
	Start      string
	End        string
}

// EditOutcome reports how one edit applied.
type EditOutcome struct {
	Match        string
	Replacements int
	// StartLine and EndLine are the 1-based lines the new text of the
	// edit's first replacement occupies in the edited file. EndLine is
	// StartLine-1 when the edit left no text.
	StartLine int
	EndLine   int
	// Excerpt is those lines and a few around them, each prefixed with its
	// anchor as AnchorLines writes it, so a follow-up anchor edit needs no
	// fresh read. Long lines are clipped, with a note of what is left out.
	Excerpt string
}

// EditRequest changes one text file of an artifact's latest version and
// publishes the result as a new version.
type EditRequest struct {
	Slug            string
	Path            string
	Edits           []Edit
	Label           string
	ExpectedVersion int  // compared with LatestVersion; 0 = no check
	KeepStale       bool // a new version normally clears stale_at/expires_at
}

// editAttempts bounds how often Edit re-applies the edits when another
// publish lands between its read and its write and the caller gave no
// ExpectedVersion.
const editAttempts = 3

// Edit applies req.Edits to a file of the latest version and publishes the
// result as a new version, carrying every other file over as Update does.
// It returns the new state, the new version and an outcome per edit.
func (s *Service) Edit(ctx context.Context, actor Actor, req EditRequest) (*Artifact, *Version, []EditOutcome, error) {
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
			continue // a publish raced the read; apply the edits to the newer content
		}
		return a, v, out, err
	}
}

func (s *Service) editOnce(ctx context.Context, actor Actor, req EditRequest, clean string) (*Artifact, *Version, []EditOutcome, error) {
	a, err := s.store.Get(ctx, req.Slug)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := s.checkWritable(actor, a, false); err != nil {
		return nil, nil, nil, err
	}
	if req.ExpectedVersion != 0 && req.ExpectedVersion != a.LatestVersion {
		return nil, nil, nil, Errorf(ErrVersionConflict, "latest version is %d, expected %d", a.LatestVersion, req.ExpectedVersion)
	}
	data, f, err := s.readLatest(ctx, a, clean)
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
	got, v, err := s.Update(ctx, actor, UpdateRequest{
		Slug:            a.Slug,
		Label:           req.Label,
		ExpectedVersion: a.LatestVersion, // the version the edits were applied to
		KeepStale:       req.KeepStale,
		Files:           []FileInput{{Path: clean, Content: strings.NewReader(edited), ContentType: f.ContentType}},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return got, v, out, nil
}

// readLatest returns the content of one text file of a's latest version.
func (s *Service) readLatest(ctx context.Context, a *Artifact, p string) (string, File, error) {
	v, err := s.store.GetVersion(ctx, a.Slug, a.LatestVersion)
	if err != nil {
		return "", File{}, err
	}
	i := slices.IndexFunc(v.Files, func(f File) bool { return f.Path == p })
	if i < 0 {
		return "", File{}, Errorf(ErrFileNotFound, "no file %q in version %d", p, v.Number)
	}
	f := v.Files[i]
	rc, err := s.blobs.Open(ctx, f.Hash)
	if err != nil {
		return "", File{}, fmt.Errorf("artifact: open %s/%s: %w", a.Slug, p, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, s.opts.MaxFileBytes+1))
	if err != nil {
		return "", File{}, fmt.Errorf("artifact: read %s/%s: %w", a.Slug, p, err)
	}
	if int64(len(data)) > s.opts.MaxFileBytes {
		return "", File{}, Errorf(ErrTooLarge, "%q is larger than the %d-byte file limit", p, s.opts.MaxFileBytes)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", File{}, Errorf(ErrInvalidArgument, "%q is not a UTF-8 text file; replace the whole file instead", p)
	}
	return string(data), f, nil
}

// ApplyEdits applies edits to content, the text of the file name (used in
// messages), and returns the edited text and an outcome per edit. Text
// edits apply in order, each to the result of the one before. Anchor edits
// all refer to content as given, must not overlap, and apply together. An
// edit that would make the text larger than maxBytes fails with
// ErrTooLarge before the text is built; 0 means no limit.
func ApplyEdits(name, content string, edits []Edit, maxBytes int64) (string, []EditOutcome, error) {
	if len(edits) == 0 {
		return "", nil, Errorf(ErrInvalidArgument, "at least one edit is required")
	}
	if len(edits) > MaxEdits {
		return "", nil, Errorf(ErrInvalidArgument, "%d edits in one call; at most %d", len(edits), MaxEdits)
	}
	anchors := 0
	for i, e := range edits {
		switch {
		case e.OldText != "" && e.Start != "":
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: give old_text or start, not both", i+1)
		case e.OldText == "" && e.Start == "":
			if e.End != "" {
				return "", nil, Errorf(ErrInvalidArgument, "edit %d: end needs a start anchor", i+1)
			}
			return "", nil, Errorf(ErrInvalidArgument,
				"edit %d: give old_text (the exact text to replace) or a start anchor from a read with anchors", i+1)
		case e.Start != "":
			anchors++
		case e.End != "":
			return "", nil, Errorf(ErrInvalidArgument, "edit %d: end applies to anchor edits; use start and end, or old_text alone", i+1)
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
	// Line numbers for every range end in one pass over the text.
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
		out[i].Excerpt = excerpt(ed.text, r, out[i].StartLine, out[i].EndLine)
	}
	return ed.text, out, nil
}

// editor holds the text being edited and, per applied edit, the byte range
// its (first) new text occupies, kept current as later edits move it.
type editor struct {
	name   string
	text   string
	max    int64 // largest text allowed; 0 = no limit
	ranges [][2]int
}

// apply replaces every match in one pass (they are sorted and do not
// overlap), moves the tracked ranges with the text, and returns the range
// each match's new text occupies. It fails before building anything if the
// result would exceed ed.max.
func (ed *editor) apply(ms []match) ([][2]int, error) {
	size := int64(len(ed.text))
	for _, m := range ms {
		size += int64(len(m.repl)) - int64(m.end-m.start)
	}
	if ed.max > 0 && size > ed.max {
		return nil, Errorf(ErrTooLarge, "the edits would make %s %d bytes, more than the %d-byte file limit", ed.name, size, ed.max)
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
	// move maps an offset of the old text into the new one; an offset
	// inside a replaced span goes to the start (or, for an end, the end) of
	// its new text, so a range that overlaps a change grows to cover it.
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

// match is one place an edit's old text was found: the byte range to
// replace and the text to put there.
type match struct {
	start, end int
	repl       string
}

func (ed *editor) applyText(n int, e Edit) (EditOutcome, error) {
	kind := MatchExact
	matches := exactMatches(ed.text, e.OldText, e.NewText)
	fuzzy := lineCount(e.OldText) <= maxFuzzyLines
	if len(matches) == 0 && fuzzy {
		kind = MatchTrailingWhitespace
		matches = lineMatches(ed.text, e, trimTrailing, false)
		if len(matches) == 0 {
			kind = MatchIndentation
			matches = lineMatches(ed.text, e, strings.TrimSpace, true)
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
	moved, err := ed.apply(matches)
	if err != nil {
		return EditOutcome{}, err
	}
	ed.ranges = append(ed.ranges, moved[0])
	return EditOutcome{Match: kind, Replacements: len(matches)}, nil
}

// exactMatches finds the non-overlapping occurrences of old in text, up to
// one more than MaxReplacements.
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

// lineMatches finds the runs of whole lines of text that equal the lines of
// e.OldText after norm, and returns them with e.NewText, re-indented to the
// file's indentation when reindent is set and given the file's line breaks.
// It is a Knuth-Morris-Pratt search over the lines, so a repetitive file
// costs linear time, and it stops after MaxReplacements+1 runs.
func lineMatches(text string, e Edit, norm func(string) string, reindent bool) []match {
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
	// fail[i] is the length of the longest proper prefix of want[:i+1]
	// that is also its suffix.
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
		first := starts[(i-n+1)%n]
		crlf := endsCRLF(text, first)
		// Replace up to the end of the last line's text, keeping its line
		// break unless the old text included one.
		end := start + len(strings.TrimSuffix(line, "\r"))
		repl := e.NewText
		if withNL {
			if start+len(line) < len(text) {
				end = start + len(line) + 1
			} else {
				repl = strings.TrimSuffix(repl, "\n")
			}
		}
		if reindent {
			repl = reindentText(repl, oldLines, splitLines(text[first:start+len(line)]))
		}
		out = append(out, match{start: first, end: end, repl: fitBreaks(repl, crlf)})
		return len(out) <= MaxReplacements
	})
	return out
}

func trimTrailing(s string) string { return strings.TrimRight(s, " \t\r") }

// leading is the indentation of s.
func leading(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// reindentText fits newText to the indentation of fileLines, the lines that
// oldLines matched ignoring indentation. Its first line takes the file's
// first line's indentation in place of the old text's. The later lines are
// shifted by the difference between the old text's and the file's first
// non-blank later line: text copied from a page often starts after its
// first line's indentation, so the first line says nothing about the rest.
// Without such a line, the first line's difference applies to all.
func reindentText(newText string, oldLines, fileLines []string) string {
	from0, to0 := leading(oldLines[0]), leading(fileLines[0])
	from, to := from0, to0
	for j := 1; j < len(oldLines) && j < len(fileLines); j++ {
		if strings.TrimSpace(oldLines[j]) != "" {
			from, to = leading(oldLines[j]), leading(fileLines[j])
			break
		}
	}
	lines := strings.Split(newText, "\n")
	for i, l := range lines {
		f, t := from, to
		if i == 0 {
			f, t = from0, to0
		}
		if f == t || strings.TrimSpace(l) == "" {
			continue
		}
		if strings.HasPrefix(l, f) {
			lines[i] = t + l[len(f):]
		}
	}
	return strings.Join(lines, "\n")
}

// endsCRLF reports whether the line starting at off ends with "\r\n".
func endsCRLF(text string, off int) bool {
	k := strings.IndexByte(text[off:], '\n')
	return k > 0 && text[off+k-1] == '\r'
}

// fitBreaks gives s the file's "\r\n" line breaks when crlf, unless s
// already uses them.
func fitBreaks(s string, crlf bool) string {
	if !crlf || strings.Contains(s, "\r\n") {
		return s
	}
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// notFound builds the edit_not_found error, naming the most similar run of
// lines so the caller can correct old_text or switch to anchors.
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

// closestCandidates is how many window starts closest scores in full for
// each of the old text's first and last lines.
const closestCandidates = 20

// near is a run of lines similar to an old text: its 0-based first line,
// its lines and a similarity from 0 to 1.
type near struct {
	start int
	lines []string
	score float64
}

// closest finds the run of lines of text most similar to old. Scoring every
// window would cost lines(text) x lines(old) comparisons, so one pass picks
// the windows whose first or last line best matches old's, and a second
// pass scores only those.
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
	var g []uint16 // reused for every line
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

// similarityKey is the part of a line similarity looks at.
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

// bigrams appends the sorted byte pairs of s to dst; a string shorter than
// two bytes is its own single "pair".
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

// anchorEdit is an anchor edit resolved to the byte range it replaces.
type anchorEdit struct {
	idx        int // position in the request
	from, to   int // 1-based lines
	start, end int // bytes
	repl       string
}

// lineSpan is one line of a text: where it starts, its content and where
// the next line starts.
type lineSpan struct {
	start int
	text  string
	next  int
}

func (ed *editor) applyAnchors(edits []Edit) ([]EditOutcome, error) {
	type parsed struct{ from, to int }
	ps := make([]parsed, len(edits))
	hashes := map[int]string{}
	for i, e := range edits {
		from, h, err := ParseAnchor(e.Start)
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
		ps[i] = parsed{from, to}
	}
	// One pass collects the anchored lines.
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
		for _, a := range []string{e.Start, e.End} {
			if a == "" {
				continue
			}
			if err := ed.checkAnchor(i+1, a, spans, total); err != nil {
				return nil, err
			}
		}
		from, to := spans[ps[i].from], spans[ps[i].to]
		repl := e.NewText
		eol := ed.text[to.start+len(to.text) : to.next] // "\n", or "" for a last line without one
		crlf := eol == "\n" && strings.HasSuffix(to.text, "\r")
		repl = fitBreaks(repl, crlf)
		if repl != "" && eol != "" && !strings.HasSuffix(repl, "\n") {
			repl += fitBreaks("\n", crlf) // the replaced lines ended with a line break
		}
		resolved = append(resolved, anchorEdit{idx: i, from: ps[i].from, to: ps[i].to, start: from.start, end: to.next, repl: repl})
	}
	byPos := slices.Clone(resolved)
	slices.SortFunc(byPos, func(a, b anchorEdit) int { return cmp.Compare(a.start, b.start) })
	for k := 1; k < len(byPos); k++ {
		if p, c := byPos[k-1], byPos[k]; c.start < p.end {
			return nil, Errorf(ErrInvalidArgument, "edits %d and %d overlap (lines %d-%d and %d-%d)",
				min(p.idx, c.idx)+1, max(p.idx, c.idx)+1, p.from, p.to, c.from, c.to)
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
		ed.ranges[a.idx] = moved[k]
	}
	out := make([]EditOutcome, len(edits))
	for i := range out {
		out[i] = EditOutcome{Match: MatchAnchor, Replacements: 1}
	}
	return out, nil
}

// checkAnchor checks anchor against the line it names. On a mismatch it
// looks for the line within 50 lines, in case it moved.
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

// clip shortens a line for an error message.
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

// lineHash is the 4-hex-digit hash of one line without its line break.
func lineHash(line string) string {
	var buf [4]byte
	return string(appendHash(buf[:0], line))
}

// appendHash appends lineHash(line): 32-bit FNV-1a, folded to 16 bits.
func appendHash(dst []byte, line string) []byte {
	v := uint32(2166136261)
	for i := 0; i < len(line); i++ {
		v ^= uint32(line[i])
		v *= 16777619
	}
	v = (v>>16 ^ v) & 0xffff
	const digits = "0123456789abcdef"
	return append(dst, digits[v>>12], digits[v>>8&0xf], digits[v>>4&0xf], digits[v&0xf])
}

// Anchor is the anchor of line number n with content line: "12:3fa0".
func Anchor(n int, line string) string {
	return string(appendAnchor(nil, n, line))
}

func appendAnchor(dst []byte, n int, line string) []byte {
	dst = strconv.AppendInt(dst, int64(n), 10)
	dst = append(dst, ':')
	return appendHash(dst, line)
}

// ParseAnchor splits "12:3fa0" into its line number and hash. Anything
// from a "|" on is ignored, so a whole line copied from AnchorLines works.
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

// AnchorLines prefixes every line of content with its anchor and "|", e.g.
// "12:3fa0|<h1>Title</h1>". The line breaks are kept; a final line break
// adds no extra line.
func AnchorLines(content string) string {
	var b strings.Builder
	_ = WriteAnchored(&b, content) // a strings.Builder does not fail
	return b.String()
}

// WriteAnchored writes AnchorLines(content) to w line by line, without
// holding the result in memory.
func WriteAnchored(w io.Writer, content string) error {
	bw := bufio.NewWriter(w)
	var prefix []byte
	eachLine(content, func(i, start int, line string) bool {
		prefix = append(appendAnchor(prefix[:0], i+1, line), '|')
		_, _ = bw.Write(prefix)
		_, _ = bw.WriteString(line)
		if start+len(line) < len(content) {
			_ = bw.WriteByte('\n')
		}
		return true
	})
	return bw.Flush() // reports the first write error
}

// excerpt renders the lines that the range r of text occupies, from
// startLine to endLine, with excerptContext lines around them, anchored. It
// reads only the lines it shows.
func excerpt(text string, r [2]int, startLine, endLine int) string {
	if text == "" {
		return ""
	}
	// The first and last byte of the new text, inside the text: an edit
	// that removed the last lines starts past the end.
	a, aLine := r[0], startLine
	if a >= len(text) {
		a = len(text) - 1
		if text[a] == '\n' {
			aLine--
		}
	}
	e, eLine := a, aLine
	if r[1] > r[0] {
		e, eLine = r[1]-1, endLine
	}
	from, back := lineStartBack(text, a, excerptContext)
	firstShown := aLine - back
	if eLine-aLine+1 <= excerptMax {
		return anchored(splitLines(text[from:lineEndForward(text, e, excerptContext)]), firstShown, 0)
	}
	half := excerptMax / 2
	head := splitLines(text[from:lineEndForward(text, a, half-1)])
	tailFrom, tailBack := lineStartBack(text, e, half-1)
	tail := splitLines(text[tailFrom:lineEndForward(text, e, excerptContext)])
	tailFirst := eLine - tailBack
	return anchored(head, firstShown, 0) + fmt.Sprintf("\n... %d lines not shown ...\n", tailFirst-firstShown-len(head)) +
		anchored(tail, tailFirst, 0)
}

// lineStartBack returns the start of the line n lines before the one
// holding off, and how many lines it went back (fewer at the top).
func lineStartBack(text string, off, n int) (int, int) {
	i := strings.LastIndexByte(text[:off], '\n') + 1
	steps := 0
	for steps < n && i > 0 {
		i = strings.LastIndexByte(text[:i-1], '\n') + 1
		steps++
	}
	return i, steps
}

// lineEndForward returns the end, before its line break, of the line n
// lines after the one holding off (or of the last line).
func lineEndForward(text string, off, n int) int {
	end := len(text)
	if k := strings.IndexByte(text[off:], '\n'); k >= 0 {
		end = off + k
	}
	for ; n > 0 && end+1 < len(text); n-- {
		next := end + 1
		end = len(text)
		if k := strings.IndexByte(text[next:], '\n'); k >= 0 {
			end = next + k
		}
	}
	return end
}

// anchored renders lines numbered from first, each as anchor|content with
// long content clipped. When skip is positive, that many lines in the
// middle are replaced by a note.
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

// clipLine shortens a long line in an excerpt and says how much is left
// out; its anchor still names the whole line.
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

// eachLine calls fn with the 0-based index, the start offset and the
// content (without its line break) of every line of text, in order, until
// fn returns false. A final line break does not start a new line.
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

// splitLines splits text at "\n"; a final line break does not start a new
// line, so "a\nb\n" is two lines and "" is none.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// lineCount is the number of lines splitLines would return.
func lineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// lineAt returns the 1-based line holding byte offset off.
func lineAt(text string, off int) int {
	return strings.Count(text[:min(off, len(text))], "\n") + 1
}

// linesAt is lineAt for many offsets in one pass over text.
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
