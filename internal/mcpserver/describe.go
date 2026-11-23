package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/csp"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/textedit"
	"github.com/PeterBooker/kazari/internal/upload"
)

// texts are generated from Options so they state the configured limits.
type texts struct {
	pageURL    string // https://<slug>.<base>/
	versionURL string // https://<slug>--v<N>.<base>/
	slugRules  string
	limits     string
	text       string
	serving    string
	csp        string
	metadata   string
	visibility string
	inline     string
	chunks     string
	archive    string

	instructions string
	tools        map[string]string
	next         map[string]string
	frontmatter  skillFrontmatter
	skill        string // SKILL.md
}

func newTexts(links present.Links, o Options) texts {
	t := texts{
		pageURL:    hostURL(links, o.BaseDomain, "<slug>"),
		versionURL: hostURL(links, o.BaseDomain, "<slug>"+artifact.VersionSeparator+"<N>"),
	}

	reserved := "none"
	if len(o.ReservedSlugs) > 0 {
		reserved = strings.Join(o.ReservedSlugs, ", ")
	}
	t.slugRules = fmt.Sprintf("Slug rules: %d to %d characters of lower-case letters a-z, digits 0-9 and hyphens; "+
		"starts and ends with a letter or digit; no \"--\" (reserved for version hosts); not one of the reserved slugs (%s). "+
		"The slug is permanent and becomes the host name: %s.",
		artifact.SlugMinLen, artifact.SlugMaxLen, reserved, t.pageURL)

	t.limits = fmt.Sprintf("Limits: each file at most %s, each version at most %s in total, at most %d files per version.",
		size(o.MaxFileBytes), size(o.MaxVersionBytes), o.MaxFiles)

	t.text = fmt.Sprintf("Text fields: name at most %d bytes and label at most %d bytes, each on one line; description "+
		"at most %d bytes (line breaks allowed); none may contain other control characters.",
		artifact.NameMaxBytes, artifact.LabelMaxBytes, artifact.DescriptionMaxBytes)

	t.serving = fmt.Sprintf("Serving: index.html is served at %s; a directory path serves the index.html inside it; "+
		"a missing path serves the artifact's own 404.html when it has one. Every version also stays reachable at %s, "+
		"so use relative links (href=\"about.html\", src=\"assets/app.js\"), never absolute paths starting with \"/\" "+
		"or links to the latest URL. Only static files are served: no server-side code runs.",
		t.pageURL, t.versionURL)

	t.csp = describeCSP(o.CSP, o.AllowCSPOverride)

	cspKey := ""
	if o.AllowCSPOverride {
		cspKey = fmt.Sprintf("; %s adds sources to the policy (see Content-Security-Policy)", artifact.MetaCSP)
	}
	t.metadata = fmt.Sprintf("Metadata is a string map. Server keys: %s=\"true\" serves index.html for any missing path "+
		"without a file extension (client-side routing); %s=\"true\" lets search engines index the page (the default "+
		"sends X-Robots-Tag: noindex)%s. Any other key (letters, digits, \"_\", \".\", \"-\", at most 64 characters) is a "+
		"free-form label that list_artifacts can filter on.",
		artifact.MetaSPA, artifact.MetaIndexable, cspKey)

	t.visibility = fmt.Sprintf("visibility is \"public\" (served to anyone, listed), \"unlisted\" (served to anyone "+
		"with the URL, not listed) or \"members\" (only people who sign in to this Kazari instance, and holders of a "+
		"share link from create_share_link, can open it); omitted, it is %q on this instance. A page is never secret "+
		"by its URL alone, so use \"members\" for anything not meant for the open web. Giving visibility needs the "+
		"artifacts:share scope; a connection without it leaves visibility out.", o.DefaultVisibility)

	t.inline = fmt.Sprintf("Files: each files[] entry is {path, content, encoding, content_type}; encoding is \"utf8\" "+
		"(default) or \"base64\" for binary files (a data: URL prefix is stripped; base64 sent without encoding is "+
		"refused for image, font, PDF, archive and media files). Inline content is for text you write yourself. A file "+
		"already on disk goes through begin_upload and archive_url whatever its size; without a shell, content above %s "+
		"goes through begin_upload and upload_chunk. Then pass upload_id here; inline files[] in the same call win on a "+
		"path clash. Only a call that succeeds consumes the upload: after an error, fix the call and resend it with the "+
		"same upload_id.", size(o.InlineGuidanceBytes))
	if o.Fetch {
		t.inline += " " + urlFile
	}

	t.archive = fmt.Sprintf("With a shell: 1) From inside the page's directory, zip -r ../site.zip . (or tar czf "+
		"../site.tar.gz .), so paths start at the page root; a single file needs no archive: use it in place of site.zip and give its path as archive_path. "+
		"2) begin_upload with archive_sha256 (sha256sum site.zip) and archive_bytes (wc -c < site.zip); the archive may "+
		"be at most %s. 3) curl --fail-with-body -T site.zip '<archive_url>': the URL needs no credential, accepts only "+
		"a body with that digest, and is spent by the first one that matches; after %d bodies that do not, the upload "+
		"is removed. 4) create_artifact or update_artifact with upload_id. If curl cannot reach the host, the sandbox's "+
		"network policy blocks it: tell the user, and fall back to upload_chunk.", size(o.MaxTotalBytes), upload.MaxArchiveAttempts)

	t.chunks = fmt.Sprintf("Offset rule: the first chunk of each path has offset 0; every next chunk's offset is the "+
		"bytes_received returned by the previous chunk of that path. Each chunk is at most %s after base64 decoding "+
		"(smaller chunks are fine); all files of one upload together at most %s. Use encoding \"base64\" for binary "+
		"files and \"utf8\" for text. If a call failed or you are unsure it arrived, resend the same chunk at the same "+
		"offset: a repeat of the last chunk is acknowledged without being written twice. On upload_offset_mismatch "+
		"the error says current=<n>: resume that path from offset n.",
		size(o.MaxChunkBytes), size(o.MaxTotalBytes))

	copyLine := ""
	if o.Fetch {
		copyLine = "- Copy of an existing page (\"like example.com, but...\"): import_page with its url and a slug, never " +
			"your own fetch of the page; then change the copy with edit_file, guided by the outline in the result.\n"
	}
	// How the tools fit together; each tool's own description says what it does.
	t.instructions = fmt.Sprintf("Kazari hosts static web pages at %s (the published version) and %s (version N). "+
		"Work in this order:\n"+
		"- New page: create_artifact, then report the url from the result to the user. If the result has warnings, "+
		"the page will not render as written: fix them.\n"+
		"%s"+
		"- Change a page: edit_file with text edits (old_text copied from the file) and expected_version set to the "+
		"latest_version you last saw, from the create, edit or read result. Read first only when you have not seen the "+
		"current text, and once you know where the change is, read just that window with start_line and end_line; ask "+
		"for anchors only for anchor edits. Send every edit to one file in one call, and never call edit_file in "+
		"parallel on one artifact. A change across files is one update_artifact call, with files[].edits or whole files. "+
		"On version_conflict or anchor_mismatch, follow the error: resend as it says, or read again and redo the edit. "+
		"After a rollback, a change needs base_version.\n"+
		"- Files on disk, or content above %s: begin_upload, then one archive sent to archive_url with a shell, or "+
		"upload_chunk for each file; then create_artifact or update_artifact with upload_id.\n"+
		"- Taking a page down: prefer set_visibility \"unpublished\" or mark_stale to delete_artifact, and ask the user "+
		"before delete_artifact with hard: true. update_artifact with restore: true undoes a soft delete.\n"+
		"- Sharing a \"members\" page with someone who cannot sign in to this instance: create_share_link, then give "+
		"the user the url from the result.\n"+
		"Errors end with the next step to take; follow it. The full guide (workflow, authoring conventions with this "+
		"instance's limits and CSP, edit modes, errors) is the skill %s, also returned by get_guide.",
		t.pageURL, t.versionURL, copyLine, size(o.InlineGuidanceBytes), skillURI)

	t.tools = t.toolDescriptions(o)
	t.next = t.nextSteps(o)
	t.frontmatter = skillFrontmatter{Name: skillName, Description: t.skillDescription(o)}
	t.skill = t.skillText(o)
	return t
}

func (t texts) toolDescriptions(o Options) map[string]string {
	dl := "<api>/artifacts/<slug>/files/<path>?version=N"
	if o.APIBaseURL != "" {
		dl = o.APIBaseURL + "/artifacts/<slug>/files/<path>?version=N"
	}
	const sameAsCreate = "Slug rules, limits, Content-Security-Policy, metadata, files and text fields: as in the " +
		"create_artifact description."
	const baseVersion = "base_version: the version the change builds on; omitted, the latest. While an earlier version " +
		"is published (after a rollback, or with a staged version) it is required, and the call fails with " +
		"published_not_latest without it: ask the user whether to change the published version or the latest. " +
		"stage: true adds the version unpublished, on the latest: url keeps serving published_version, and " +
		"version_url opens only to signed-in members until publish_version."
	return map[string]string{
		"create_artifact": strings.Join([]string{
			fmt.Sprintf("Create a new artifact (a static web page or site) and publish it as version 1 at %s. "+
				"Returns url, version, version_url, latest_version and the artifact's state; report the url to the user. "+
				"warnings, when present, name what will stop the page from rendering as published (no index.html, "+
				"a missing file, an origin the policy blocks): fix them with update_artifact.", t.pageURL),
			t.slugRules, t.limits, t.serving, t.csp, t.metadata, t.inline,
			t.visibility,
			"label is an optional note on the version.",
			t.text,
		}, "\n\n"),

		"import_page": strings.Join([]string{
			fmt.Sprintf("Copy an existing web page into a new artifact and publish it as version 1 at %s. The server "+
				"fetches the page and its stylesheets (with their url() and @import references), fonts, images, srcset "+
				"candidates, icons and media, stores them under assets/ with relative references, makes navigation links "+
				"and form actions absolute to the source site, and removes the page's scripts and event handlers (the "+
				"copy is static, and the source's analytics must not run here); keep_scripts: true keeps them. A "+
				"resource stays remote only when the Content-Security-Policy names its host.", t.pageURL),
			"Use it whenever the user wants a page copied, cloned or used as the starting point for changes (\"like " +
				"example.com, but with a new hero\"), instead of fetching the page yourself: the copy keeps the markup " +
				"and styling that a text fetch loses. name and description default to the page's title and meta " +
				"description. Only import pages the user has the right to copy.",
			"The result's import field reports the source, how many files were fetched, which resources stayed remote " +
				"and why (skipped), how many scripts were removed, notes (a page that renders with JavaScript imports as " +
				"a shell), and an outline: each landmark (header, nav, main, section, article, aside, footer, form, h1, " +
				"h2) with the line its opening tag is on, that tag as text, and its first heading. To change the copy " +
				"use edit_file: a text edit whose old_text is an outline tag replaces that element's start, so new_text " +
				"of new content followed by the same tag inserts before it; on a page kept on few lines, read a window " +
				"around the landmark's line with read_file start_line and end_line first.",
			"A page behind a login, a CAPTCHA or bot protection fails with fetch_failed: ask the user to save it from " +
				"their browser and publish the files instead.",
			"Slug rules, visibility, limits and text fields: as in the create_artifact description. label is an " +
				"optional note on the version.",
		}, "\n\n"),

		"update_artifact": strings.Join([]string{
			fmt.Sprintf("Update an existing artifact. With files (or upload_id) it adds one new version, published at "+
				"once at %s: a files[] entry with content (or url) adds or replaces a path, {path, edits} changes a text "+
				"file with edits as in edit_file, and {path, delete: true} removes a path; every file not mentioned "+
				"carries over. Use edits per file for a change across files, so one call makes one version. Without files "+
				"only the fields change and no version is added; when the files match the latest version, the result says "+
				"unchanged and no version is added.", t.pageURL),
			"name, description and visibility (\"public\", \"unlisted\", \"members\" or \"unpublished\", as in " +
				"set_visibility) replace the stored values when given; visibility needs the artifacts:share scope. " +
				"metadata is merged into the stored map; a key set to null is deleted.",
			"expected_version: pass the latest_version you last saw. If someone published since, the call fails with " +
				"version_conflict and changes nothing. Omit it to skip the check. " + baseVersion,
			"restore: true brings back a soft-deleted artifact (get_artifact shows deleted_at) and then applies the " +
				"other changes; without it a soft-deleted artifact is not_found. A new version, or a restore, clears a " +
				"pending stale mark unless keep_stale is true.",
			"To make the new version hold exactly a set of files, list the latest version's paths with get_artifact " +
				"and include_files, and add {path, delete: true} for each path not in the set.",
			sameAsCreate,
		}, "\n\n"),

		"edit_file": strings.Join([]string{
			fmt.Sprintf("Change one text file of an artifact with edits and publish the result as a new version at %s; "+
				"every other file carries over. Prefer it to update_artifact for a change inside one file: only the "+
				"changed text is sent. Send every edit to the file in one call, and never call edit_file in parallel on "+
				"one artifact: each call makes a version, and the next one fails with version_conflict. For several "+
				"files, use update_artifact with edits per file.", t.pageURL),
			"Text edits (the default): {old_text, new_text}. old_text is copied exactly from the file and must match once (add " +
				"surrounding lines, or set replace_all). If it does not match exactly, whole lines are compared ignoring " +
				"trailing whitespace, then ignoring indentation (new_text is re-indented to fit); each edit's match in the " +
				"result says which was used. Edits apply in order. new_text omitted or empty deletes. A miss names the " +
				"closest lines with their anchors.",
			"Anchor edits: {start, end, new_text} with anchors (\"12:3fa0\") from read_file with anchors: true or from an " +
				"earlier edit's excerpt. The lines from start to end (inclusive; end defaults to start) are replaced by " +
				"new_text. {after, new_text} or {before, new_text} inserts new_text as new lines after or before the " +
				"anchored line. All anchors in a call refer to the file as read and must not overlap; a line that changed " +
				"fails with anchor_mismatch and nothing is written. Anchors cost a read and pay off only when you do not " +
				"have the exact text of a long block, when the text repeats in the file, or after a text edit missed.",
			fmt.Sprintf("One call takes text edits or anchor edits, not both, at most %d. expected_version: the "+
				"latest_version you last saw; if someone published since, the call fails with version_conflict, which "+
				"says whether every edit still applies to the newer version and the number to resend with. Without it, "+
				"edits that still apply are applied to the newest content. %s", textedit.MaxEdits, baseVersion),
			fmt.Sprintf("The result has the new version, version_url, latest_version, warnings when the page will not "+
				"render as written and, per edit, start_line, end_line and an anchored excerpt of the new text with %d "+
				"lines around it, so a follow-up anchor edit needs no new read. In a new text of more than %d lines that "+
				"you sent as is, the lines between its first and last are bare anchors (\"9:395c\"). Edits whose excerpts "+
				"would overlap share one: excerpt_of names the edit that holds it. A new version clears a pending stale "+
				"mark unless keep_stale is true. Limits: as in the create_artifact description.",
				textedit.ExcerptContext, textedit.ExcerptFullLines),
		}, "\n\n"),

		"get_guide": "Return the guide to this Kazari instance: the workflow, how to edit files, the authoring " +
			"conventions with this instance's limits and Content-Security-Policy, and what to do after each error. It is " +
			"the Agent Skill also served as the resource " + skillURI + ". Read it once before publishing or editing " +
			"if your client has not loaded that skill.",

		"begin_upload": strings.Join([]string{
			fmt.Sprintf("Start an upload, for files already on disk (any size) and content too large to inline (any "+
				"file, or all files together, above %s). Returns upload_id, expires_at, max_chunk_bytes and "+
				"max_total_bytes, and with archive_sha256 and archive_bytes also archive_url.", size(o.InlineGuidanceBytes)),
			t.archive,
			"Without a shell: 1) begin_upload. 2) upload_chunk for each file, following its offset rule. 3) " +
				"create_artifact or update_artifact with upload_id (plus any small inline files or delete entries).",
			"Only a publishing call that succeeds consumes the upload; after an error, correct the call and send it " +
				"again with the same upload_id. discard_upload drops an upload you no longer need; unused ones expire at expires_at.",
		}, "\n\n"),

		"discard_upload": "Discard an upload started with begin_upload that you no longer need, removing its staged " +
			"files. Use it after changing your mind, or when begin_upload fails with too_many_uploads. An upload is " +
			"also removed by the create_artifact or update_artifact call that publishes it, and expires at its " +
			"expires_at on its own.",

		"upload_chunk": strings.Join([]string{
			"Append one chunk of one file to an upload started with begin_upload. path is the file's path in the " +
				"artifact (e.g. assets/video.mp4); content_type is taken from the first chunk of a path.",
			t.chunks,
			"When every file is staged, call create_artifact or update_artifact with upload_id.",
		}, "\n\n"),

		"get_artifact": fmt.Sprintf("Get an artifact's record (url, owner, visibility, metadata, published_version, latest_version, "+
			"size, stale/expiry and deletion state) and its versions, newest first, each with its pinned URL. "+
			"include_files adds file lists (path, size, sha256 hash, content_type), newest version first and up to "+
			"%d files in one result; next_files_before, when present, is the files_before that gets the next page. "+
			"Use latest_version as expected_version for update_artifact and edit_file; their results, and "+
			"create_artifact's, carry it too.", artifact.MaxListedFiles),

		"read_file": fmt.Sprintf("Read one file of an artifact. version defaults to the published version. The text "+
			"starts with a line naming the path, the version read and, when it is not the one an edit changes by "+
			"default, the base_version an edit of it needs. Text files up to %s come back inline; other files up to %s "+
			"as a base64 blob resource; anything larger as a download URL on the REST API (%s, bearer authentication). "+
			"anchors: true prefixes every line of a text file with its anchor and \"|\" (\"12:3fa0|<h1>Title</h1>\") for "+
			"edit_file's anchor edits; text edits need no anchors. start_line and end_line (1-based, inclusive; "+
			"end_line defaults to the end) read only those lines of any text file up to %s, with the same anchors when "+
			"asked; use them when an earlier read, an edit result or an import outline already says where the change "+
			"is. A window holds as many whole lines as fit %s and says where to continue when it was cut short. The "+
			"structured result carries path, content_type, size, hash (sha256), encoding (utf8, base64 or url), "+
			"version, latest_version, the window's lines and total_lines, and for text the content.",
			size(o.MaxInlineReadBytes), size(MaxInlineBlobBytes), dl, size(o.MaxFileBytes), size(o.MaxInlineReadBytes)),

		"list_artifacts": "List artifacts visible to you: every public and members artifact plus unlisted ones you " +
			"created or updated. Filters: mine = \"created\", \"updated\" or \"any\" (relative to you); metadata = key/value pairs " +
			"that must all match; stale = \"include\" (default), \"exclude\" or \"only\"; deleted = \"exclude\" (default), " +
			"\"include\" or \"only\" for soft-deleted artifacts you may still see (your own, or ones you worked on), which " +
			"update_artifact with restore: true brings back. limit defaults to 50, at most 200; when next_cursor is set, " +
			"pass it as cursor for the next page.",

		"publish_version": fmt.Sprintf("Point %s at an existing version: roll back or forward, or make a staged version live. Versions are numbered "+
			"from 1 to latest_version (get_artifact lists them); nothing is copied or deleted, and the version keeps its "+
			"own URL %s. While an earlier version than the latest is published, edit_file and update_artifact need "+
			"base_version.", t.pageURL, t.versionURL),

		"mark_stale": "Mark an artifact stale. The page keeps serving, but unless it is updated or unmarked before " +
			"expires_at it is soft-deleted, then purged after the retention period. grace is a duration such as \"72h\", " +
			"\"14d\" or \"2w\"; omit it for the instance default. On an artifact you do not own it needs the artifacts:delete scope.",

		"unmark_stale": "Cancel a pending stale expiry, keeping the artifact indefinitely. Publishing a new version " +
			"also cancels it unless keep_stale is set.",

		"set_visibility": fmt.Sprintf("Set an artifact's visibility. \"public\": served to anyone and listed. "+
			"\"unlisted\": served to anyone with the URL but not listed. \"members\": served only to people who sign "+
			"in to this Kazari instance (a browser is sent to sign in first) and to holders of a share link "+
			"(create_share_link), on the latest URL and every version URL; listed to members. \"unpublished\": takes "+
			"the page offline (%s answers 404) while keeping every version; setting \"public\", \"unlisted\" or "+
			"\"members\" brings it back online. There is no \"private\": use \"members\". A page is never secret by "+
			"its URL alone: slugs are easy to guess, so use \"members\" for anything not meant for the open web. The "+
			"result says who can open the page now. Needs the artifacts:share scope.", t.pageURL),

		"create_share_link": fmt.Sprintf("Create a share link: a URL that opens an artifact in a browser without "+
			"signing in, for someone outside this instance (a client, a reviewer, a headless browser). It matters for "+
			"\"members\" artifacts; public and unlisted ones open for anyone anyway. The url opens the published version "+
			"until expires_in has passed (a duration such as \"72h\", \"14d\" or \"2w\"; default 7 days, at most %s) "+
			"or the link is revoked; with version, version_url opens that version (%s). label is a note such as who "+
			"the link is for. The URLs hold the link's secret and are returned only this once: give them to the user. "+
			"Anyone who has one can open the page, so it is for the people it is meant for only; with password "+
			"(%d to %d characters) it asks for that too. Needs the artifacts:share scope.",
			days(o.MaxShareLinkTTL), hostOnly(t.versionURL), artifact.ShareLinkPasswordMin, artifact.ShareLinkPasswordMax),

		"list_share_links": "List an artifact's share links, newest first: prefix, label, created_by, expires_at, " +
			"revoked_at, uses, last_used_at and whether each is active. Their URLs are never shown again; revoke a link " +
			"by its prefix with revoke_share_link.",

		"revoke_share_link": "Revoke one share link of an artifact by its prefix (from list_share_links or " +
			"create_share_link). It stops opening the page at once: browsers that opened it lose access on their next " +
			"request. Revoking twice is fine. Needs the artifacts:share scope.",

		"delete_artifact": "Delete an artifact. By default a soft delete: the page goes offline at once and the " +
			"artifact is purged after the retention period. hard = true purges it now, permanently: every version is " +
			"gone and the slug stays blocked for a while. Ask the user before a hard delete; clients that support it are " +
			"asked to confirm, and if the user declines nothing is deleted. Prefer set_visibility \"unpublished\" to " +
			"take a page offline reversibly. A soft delete is undone with update_artifact and restore: true until the purge.",
	}
}

func (t texts) promptText(o Options, topic string) string {
	var b strings.Builder
	if topic != "" {
		fmt.Fprintf(&b, "Create a web page about: %s\n\nThen publish it to Kazari with the create_artifact tool.\n\n", topic)
	} else {
		b.WriteString("Create a web page and publish it to Kazari with the create_artifact tool.\n\n")
	}
	b.WriteString("Conventions:\n")
	fmt.Fprintf(&b, "- Build a single self-contained index.html; it is served at %s. Extra files (CSS, JavaScript, images) "+
		"are fine: reference them with relative links such as \"assets/app.js\", never paths starting with \"/\", so the "+
		"page also works on pinned version URLs (%s).\n", t.pageURL, t.versionURL)
	fmt.Fprintf(&b, "- Put CSS and JavaScript inline, or load libraries only from the CDNs the Content-Security-Policy "+
		"allows. %s\n", t.csp)
	b.WriteString("- No server-side code: Kazari serves static files only. There is no PHP, no API route, no database and " +
		"no build step on the server; ship the built output.\n")
	fmt.Fprintf(&b, "- Pick a short, descriptive slug from the topic, e.g. \"q3-roadmap\". %s If create_artifact fails "+
		"with slug_taken, choose another slug.\n", t.slugRules)
	fmt.Fprintf(&b, "- %s If any file, or all files together, exceed %s, use begin_upload and upload_chunk, then call "+
		"create_artifact with upload_id.\n", t.limits, size(o.InlineGuidanceBytes))
	fmt.Fprintf(&b, "- For client-side routing set metadata %s = \"true\".\n", artifact.MetaSPA)
	b.WriteString("- When the page is published, report the url from the result back to the user.\n")
	return b.String()
}

// hostURL prefers links: they know the public scheme and port, e.g. in development.
func hostURL(links present.Links, base, sub string) string {
	if links.URL != nil {
		return links.URL(sub)
	}
	if base != "" {
		return "https://" + sub + "." + base + "/"
	}
	return "https://" + sub + ".<base>/"
}

func size(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// cspDirectives are explained in this order; optional ones only when the policy names them.
var cspDirectives = []struct {
	name, what string
	optional   bool
}{
	{"script-src", "scripts", false},
	{"style-src", "styles", false},
	{"font-src", "fonts", false},
	{"img-src", "images", false},
	{"connect-src", "fetch/XHR/WebSocket", false},
	{"media-src", "audio and video", true},
	{"frame-src", "frames", false},
}

func describeCSP(csp string, override bool) string {
	csp = strings.TrimSpace(csp)
	if csp == "" {
		return "Content-Security-Policy: none is sent, so pages may load resources from anywhere."
	}
	dirs := parseCSP(csp)
	var parts []string
	for _, d := range cspDirectives {
		src, ok := dirs[d.name]
		if !ok && d.optional {
			continue
		}
		if !ok {
			src, ok = dirs["default-src"]
		}
		if !ok {
			parts = append(parts, d.what+" from anywhere")
			continue
		}
		parts = append(parts, d.what+" from "+describeSources(src))
	}
	var b strings.Builder
	b.WriteString("Content-Security-Policy (CSP) on every page allows ")
	b.WriteString(strings.Join(parts, "; "))
	b.WriteString(".")
	if fa, ok := dirs["frame-ancestors"]; ok && len(fa) == 1 && fa[0] == "'none'" {
		b.WriteString(" Pages cannot be embedded in frames.")
	}
	b.WriteString(" The browser blocks anything else: inline your code or ship libraries as files")
	if override {
		fmt.Fprintf(&b, ", or add origins with metadata %s, which adds sources to the fetch directives above and "+
			"never removes any (e.g. \"frame-src https://www.youtube.com\")", artifact.MetaCSP)
	} else {
		fmt.Fprintf(&b, "; this instance does not take %s metadata", artifact.MetaCSP)
	}
	b.WriteString(". A publish's warnings name blocked origins and missing files.")
	return b.String()
}

func parseCSP(policy string) map[string][]string { return csp.Parse(policy) }

// urlFile describes files[].url where fetching is on.
const urlFile = "A file can give url instead of content: an http or https address on a public host, without " +
	"credentials, that the server downloads as the file's content, up to the file limit and, all of a call's url " +
	"files together, within the version limit and the fetch budget (content_type is taken from the response when " +
	"omitted). A download that fails fails the whole call with fetch_failed, and nothing is published."

func describeSources(src []string) string {
	if len(src) == 0 {
		return "nowhere"
	}
	var out []string
	for _, s := range src {
		switch strings.ToLower(s) {
		case "'none'":
			return "nowhere"
		case "'self'":
			out = append(out, "own files")
		case "'unsafe-inline'":
			out = append(out, "inline")
		case "'unsafe-eval'":
			out = append(out, "eval")
		case "data:":
			out = append(out, "data: URIs")
		case "blob:":
			out = append(out, "blob: URLs")
		case "https:":
			out = append(out, "any https: URL")
		case "*":
			out = append(out, "anywhere")
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// days prints a whole number of days as "90 days", anything else as a Go
// duration.
func days(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d == day:
		return "1 day"
	case d%day == 0:
		return fmt.Sprintf("%d days", d/day)
	}
	return d.String()
}

// hostOnly is the host of a URL pattern, e.g. <slug>--v<N>.art.example.com
// from https://<slug>--v<N>.art.example.com/.
func hostOnly(u string) string {
	if _, rest, ok := strings.Cut(u, "://"); ok {
		u = rest
	}
	return strings.TrimSuffix(u, "/")
}
