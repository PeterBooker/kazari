package mcpserver

import (
	"fmt"
	"strings"

	"kazari/internal/artifact"
	"kazari/internal/present"
)

// texts are the descriptions, instructions and prompt conventions, all
// generated from Options so they always state the configured limits.
type texts struct {
	pageURL    string // https://<slug>.<base>/
	versionURL string // https://<slug>--v<N>.<base>/
	slugRules  string
	limits     string
	text       string
	serving    string
	csp        string
	metadata   string
	inline     string
	chunks     string

	instructions string
	tools        map[string]string
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

	t.csp = describeCSP(o.CSP)

	t.metadata = fmt.Sprintf("Metadata is a string map. Keys the server interprets: %s=\"true\" serves index.html for any "+
		"missing path without a file extension (client-side routing); %s=\"true\" lets search engines index the page "+
		"(the default sends X-Robots-Tag: noindex); %s replaces the Content-Security-Policy for this artifact when the "+
		"instance allows overrides. Any other key (letters, digits, \"_\", \".\", \"-\", at most 64 characters) is a "+
		"free-form label that list_artifacts can filter on.",
		artifact.MetaSPA, artifact.MetaIndexable, artifact.MetaCSP)

	t.inline = fmt.Sprintf("Inline files: give each file in files[] as {path, content, encoding, content_type}; encoding is "+
		"\"utf8\" (default, for text) or \"base64\" (for binary). When any single file or all files together exceed %s, "+
		"do not inline them: call begin_upload, send the content with upload_chunk, then pass upload_id here. Inline "+
		"files[] given in the same call are merged with the staged files and win on a path clash. The upload is consumed "+
		"by this call whether it succeeds or fails.",
		size(o.InlineGuidanceBytes))

	t.chunks = fmt.Sprintf("Offset rule: the first chunk of each path has offset 0; every next chunk's offset is the "+
		"bytes_received returned by the previous chunk of that path. Each chunk is at most %s after base64 decoding "+
		"(smaller chunks are fine); all files of one upload together at most %s. Use encoding \"base64\" for binary "+
		"files and \"utf8\" for text. If a call failed or you are unsure it arrived, resend the same chunk at the same "+
		"offset: a repeat of the last chunk is acknowledged without being written twice. On upload_offset_mismatch "+
		"the error says current=<n>: resume that path from offset n.",
		size(o.MaxChunkBytes), size(o.MaxTotalBytes))

	t.instructions = fmt.Sprintf("Kazari hosts static web pages (HTML artifacts). Each artifact has a slug and is "+
		"served at %s (the published version) and %s (a pinned version). Publish a new page with create_artifact and "+
		"change it with update_artifact; for content above %s use begin_upload and upload_chunk, then pass upload_id. "+
		"Inspect with get_artifact, read_file and list_artifacts. publish_version rolls back or forward, "+
		"set_visibility takes a page offline or back, mark_stale schedules expiry, delete_artifact removes it. "+
		"The publish-page prompt carries the authoring conventions. Always report the URL from a result to the user.",
		t.pageURL, t.versionURL, size(o.InlineGuidanceBytes))

	t.tools = t.toolDescriptions(o)
	return t
}

func (t texts) toolDescriptions(o Options) map[string]string {
	dl := "<api>/artifacts/<slug>/files/<path>?version=N"
	if o.APIBaseURL != "" {
		dl = o.APIBaseURL + "/artifacts/<slug>/files/<path>?version=N"
	}
	return map[string]string{
		"create_artifact": strings.Join([]string{
			fmt.Sprintf("Create a new artifact (a static web page or site) and publish it as version 1 at %s. "+
				"Returns url, version and the artifact record; report the url to the user.", t.pageURL),
			t.slugRules, t.limits, t.serving, t.csp, t.metadata, t.inline,
			"visibility is \"public\" (default, appears in listings) or \"unlisted\" (served to anyone with the URL, not listed). " +
				"label is an optional note on the version.",
			t.text,
		}, "\n\n"),

		"update_artifact": strings.Join([]string{
			fmt.Sprintf("Update an existing artifact. With files (or upload_id) it adds a new version that is published "+
				"at once at %s: files[] entries add or replace paths, {path, delete: true} removes a path, and every file "+
				"not mentioned carries over from the latest version. Without files only the fields change and no version "+
				"is added.", t.pageURL),
			"name, description and visibility (\"public\" or \"unlisted\") replace the stored values when given. " +
				"metadata is merged into the stored map; a key set to null is deleted.",
			"expected_version: pass the latest_version you last saw (from get_artifact or a previous result). If " +
				"someone published since, the call fails with version_conflict and changes nothing; fetch the artifact " +
				"again, re-apply your change and retry. Omit it to skip the check.",
			"A new version clears a pending stale mark unless keep_stale is true.",
			t.text, t.limits, t.csp, t.metadata, t.inline,
		}, "\n\n"),

		"begin_upload": strings.Join([]string{
			fmt.Sprintf("Start a chunked upload, for content too large to inline (any file, or all files together, "+
				"above %s). Returns upload_id, expires_at, max_chunk_bytes and max_total_bytes.", size(o.InlineGuidanceBytes)),
			"Recipe: 1) begin_upload. 2) For each file call upload_chunk with offset 0, then with offset = the " +
				"bytes_received of the previous chunk, until the whole file is sent. 3) Call create_artifact or " +
				"update_artifact with upload_id (plus any small inline files or delete entries). That call consumes the " +
				"upload whether it succeeds or fails; begin a new upload to retry. Unused uploads expire at expires_at.",
			t.chunks,
		}, "\n\n"),

		"upload_chunk": strings.Join([]string{
			"Append one chunk of one file to an upload started with begin_upload. path is the file's path in the " +
				"artifact (e.g. assets/video.mp4); content_type is taken from the first chunk of a path.",
			t.chunks,
			"When every file is staged, call create_artifact or update_artifact with upload_id.",
		}, "\n\n"),

		"get_artifact": "Get an artifact's record (url, owner, visibility, metadata, published_version, latest_version, " +
			"size, stale/expiry and deletion state) and its versions, newest first, each with its pinned URL. " +
			"include_files adds every version's file list (path, size, sha256 hash, content_type). Use latest_version " +
			"as expected_version for update_artifact.",

		"read_file": fmt.Sprintf("Read one file of an artifact. version defaults to the published version. Text files up "+
			"to %s come back inline as text; other files up to %s come back as a base64 blob resource; anything larger "+
			"returns a download URL on the REST API (%s, bearer authentication). The structured result always "+
			"carries path, content_type, size, hash (sha256) and encoding (utf8, base64 or url).",
			size(o.MaxInlineReadBytes), size(MaxInlineBlobBytes), dl),

		"list_artifacts": "List artifacts visible to you: every public artifact plus unlisted ones you created or " +
			"updated. Filters: mine = \"created\", \"updated\" or \"any\" (relative to you); metadata = key/value pairs " +
			"that must all match; stale = \"include\" (default), \"exclude\" or \"only\". limit defaults to 50, at most " +
			"200; when next_cursor is set, pass it as cursor for the next page.",

		"publish_version": fmt.Sprintf("Point %s at an existing version, to roll back or forward. Versions are numbered "+
			"from 1 to latest_version (get_artifact lists them); nothing is copied or deleted, and the version keeps its "+
			"own URL %s.", t.pageURL, t.versionURL),

		"mark_stale": "Mark an artifact stale. The page keeps serving, but unless it is updated or unmarked before " +
			"expires_at it is soft-deleted, then purged after the retention period. grace is a duration such as \"72h\" " +
			"or \"14d\"; omit it for the instance default.",

		"unmark_stale": "Cancel a pending stale expiry, keeping the artifact indefinitely. Publishing a new version " +
			"also cancels it unless keep_stale is set.",

		"set_visibility": fmt.Sprintf("Set an artifact's visibility. \"public\": served and listed. \"unlisted\": served "+
			"to anyone with the URL but not listed. \"unpublished\": takes the page offline (%s answers 404) while "+
			"keeping every version; setting \"public\" or \"unlisted\" brings it back online.", t.pageURL),

		"delete_artifact": "Delete an artifact. By default a soft delete: the page goes offline at once and the " +
			"artifact is purged after the retention period. hard = true purges it now, permanently: every version is " +
			"gone and the slug stays blocked for a while. Prefer set_visibility \"unpublished\" to take a page offline " +
			"reversibly.",
	}
}

// promptText is the body of the publish-page prompt.
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

// hostURL renders the root URL of a host label for descriptions.
func hostURL(links present.Links, base, sub string) string {
	if base != "" {
		return "https://" + sub + "." + base + "/"
	}
	if links.URL != nil {
		return links.URL(sub)
	}
	return "https://" + sub + ".<base>/"
}

// size renders a byte count the way a person writes it: 2 MiB, 256 KiB.
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

// cspDirectives are the directives worth explaining to an author, in order,
// with the noun used for them. Optional ones are described only when the
// policy names them.
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
	{"frame-src", "frames", true},
}

// describeCSP explains a Content-Security-Policy in a sentence per
// directive, naming the CDN hosts verbatim, and quotes the policy.
func describeCSP(csp string) string {
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
	b.WriteString(" Anything else is blocked by the browser: inline your code or ship libraries as files. Policy: ")
	b.WriteString(csp)
	return b.String()
}

// parseCSP splits a policy into directive name -> source list. A repeated
// directive keeps its first occurrence, as browsers do.
func parseCSP(csp string) map[string][]string {
	out := map[string][]string{}
	for _, d := range strings.Split(csp, ";") {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		name := strings.ToLower(f[0])
		if _, dup := out[name]; !dup {
			out[name] = f[1:]
		}
	}
	return out
}

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
