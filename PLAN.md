# Agalma implementation plan

Agalma is a self-hosted artifact host: one Go binary that serves every artifact as a static site at
`<slug>.<base-domain>` under a single wildcard certificate, with a REST API and an MCP server for
creating and updating artifacts, a separate admin API, and a scheduler that retires stale artifacts.

This document turns the build plan in the Claude Docs document "Open-source Artifacts host — build
plan" (dated 2026-10-23) into an implementation plan for this repository. Section 1 records where this
plan departs from that document and why. Everything after it is the plan to build against.

Module path: `agalma`, as the current `go.mod` declares (Go 1.27.3). Distribution is release
binaries and a Docker image, so `go install` from source is not a goal. Licence: MIT.

---

## 1. Review of the source plan

The source plan is sound on product shape: subdomain per artifact, one wildcard cert, immutable
content-addressed versions, reversible lifecycle, MCP and REST sharing one service layer, admin on a
separate host. All of that stays. The changes below are about reducing surface area for v1, fixing a
few details that would not survive implementation, and recording decisions the source left open.

### Decisions recorded

1. **Language is Go.** The Rust-vs-Go spike (source Phase 0) is dropped; it would cost one to two
   weeks to confirm a decision already made. The reasons from the source table still hold: official
   MCP Go SDK, CertMagic plus libdns for DNS-01, team fit. Record it as `docs/adr/0001-language.md`.
2. **Name is Agalma.** Every `forge.` / `forge://` / `forge` in the source becomes `agalma.`
   (metadata prefix), `agalma://` (MCP resource scheme) and `agalma` (binary and CLI).
3. **No DI framework, no web framework, no ORM.** Constructor injection wired in `internal/app`,
   `net/http` with Go 1.22+ `ServeMux` patterns, `database/sql` with hand-written SQL. See section 3.
4. **Licence is MIT**, as already in the repo. Apache-2.0's patent grant was considered and not
   taken; revisit only if a client contract asks for it.
5. **Default write policy is `shared`**: any member with `artifacts:write` may update any artifact,
   which is what "list what I created and/or updated" implies for a team. `owner` stays available
   in config for a multi-tenant instance.
6. **Users sign in with their organisation's identity provider.** The OAuth consent flow for
   claude.ai authenticates the person through standard OpenID Connect against the instance's
   configured provider (Microsoft Entra ID, Google Workspace, Okta, Keycloak, Authentik or any
   other OIDC-discoverable issuer). No passwords are stored. An instance with no provider
   configured falls back to pasting an API key on the consent page. Microsoft Entra ID is the first
   provider the Phase 2b gate runs against, and `auto_provision` is on by default so a colleague
   with a verified email in an allowed domain gets an account on first sign-in. Enterprise Managed
   Auth stays in Phase 4, unscheduled. Details in section 9.
7. **Large bundles reach the server in chunks.** Inline JSON files in a tool call are fine to a few
   megabytes; above that, an MCP client with no shell (claude.ai) needs another path. Two tools,
   `begin_upload` and `upload_chunk`, stage files on the server; `create_artifact` and
   `update_artifact` then take an `upload_id` instead of, or alongside, inline `files[]`. The REST
   API gets the same three endpoints so the 1:1 rule holds. Details in sections 5 and 8.

### Changes to the design

8. **OAuth is still needed for claude.ai, but not for the first MCP gate.** Anthropic's connector docs
   list a fixed bearer header (`static_headers`) as a beta limited to some organisations, so a
   self-hosted instance cannot rely on it. Claude Code, however, takes a bearer header today
   (`claude mcp add --transport http --header`). Phase 2 therefore splits: 2a ships the MCP server
   with API-key auth and gates on Claude Code; 2b adds the OAuth 2.1 authorization server and gates on
   claude.ai. The authorization server must support PKCE S256, Dynamic Client Registration *and*
   Client ID Metadata Documents (Claude Code identifies itself with a CIMD and a port-agnostic loopback
   redirect; claude.ai uses CIMD when advertised, else DCR), refresh-token rotation, a
   form-urlencoded token endpoint, and RFC 9728 protected-resource metadata behind a `401`.
9. **The authorization server federates to an upstream OIDC provider rather than owning
   credentials.** Agalma is an OAuth 2.1 authorization server towards MCP clients and an OpenID
   Connect relying party towards the organisation's identity provider. Users are matched by
   `(issuer, subject)`, linked by verified email to admin-created accounts, or provisioned on first
   sign-in when their email domain is allow-listed. Enterprise Managed Auth (Claude's silent SSO
   exchange, RFC 7523) needs a Team or Enterprise plan and a pre-registered client, so it is a
   Phase 4 item; supporting Client ID Metadata Documents in Phase 2b keeps that door open.
10. **MCP transport runs stateless.** The Go SDK's `StreamableHTTPOptions{Stateless: true}` builds a
   server per request and speaks protocol 2026-07-28; our tools hold no session state, so there is
   nothing to lose and no session store to maintain. The per-request server factory also gives tool
   handlers the authenticated principal as a plain closure variable.
11. **The version manifest becomes rows, not JSON.** `version.manifest` (a JSON blob of path to hash)
   is replaced by a `version_file` table. File lookup on the serving path is one indexed query,
   blob garbage collection is an anti-join, and no JSON is parsed on the hot path. Artifact
   `metadata` stays a JSON text column because it is opaque and only needs `json_extract` filters.
12. **No in-memory slug index in v1.** A SQLite point lookup through the pure-Go driver costs tens of
   microseconds; it is not the bottleneck for a host serving LLM-written pages. The serving handler
   depends on a small `Resolver` interface so a caching decorator can be added later without touching
   handlers. Versions are immutable, so a version's file list is cacheable forever; only
   slug to published-version needs invalidation. Add the cache when a measurement asks for it.
13. **Version pinning is by host only.** `<slug>--v3.<base>` stays; `?v=` is dropped. Two mechanisms
   mean two cache policies on one URL space and relative links that break between them. To keep the
   host form unambiguous, slugs may not contain `--`.
14. **Tombstones are a column, not a table.** A purged artifact keeps its `artifact` row with
    `purged_at` set and no versions; the existing unique index on `slug` keeps the slug reserved
    until the row is deleted after the tombstone period. One fewer table, one fewer code path.
15. **Runtime-mutable settings are deferred.** The admin `PATCH /settings` endpoint implies a
    settings table, a merge with file config, and a cache. v1 reads one config file (plus
    environment overrides) and `GET /settings` returns the effective config. Changing a limit means
    editing the file and restarting; the service restarts in under a second.
16. **Server-side HTML injection is out of v1.** `forge.title`, `forge.og_image` and the stale banner
    all require rewriting HTML on the serving path, which breaks `http.ServeContent` (ranges, ETags,
    conditional requests) and content addressing. Authors own their `<head>`. The artifact's `name`
    and `description` feed the API and the future gallery, not the page. The stale banner moves to
    Phase 4 as a header-plus-optional-injection feature.
17. **Per-artifact CSP override is allowed verbatim, behind an instance flag.** Validating that an
    override only "narrows" a CSP is a parser project. Because each artifact is its own origin, a
    looser CSP affects only that artifact's visitors. `agalma.csp` replaces the instance default when
    `serve.allow_csp_override = true` (the default for a private instance).
18. **`X-Robots-Tag: noindex` is the default regardless of visibility**, overridden only by
    `agalma.indexable = "true"`. `visibility` then controls listing and future gating, not robots,
    which keeps the two concerns separate.
19. **Write policy is one config key and one function.** `shared` (the decided default) lets any
    member with `artifacts:write` update any artifact; `owner` restricts updates to the owner and
    admins. Nothing else in the code knows which policy is active.
20. **The stdio MCP proxy is dropped from v1.** Claude Code and the other target clients speak
    Streamable HTTP. Anyone who needs stdio can use a generic bridge.
21. **Binary size target is 25 MB, not 10 MB.** Both pure-Go SQLite options (a Wasm build on wazero,
    or the transpiled C) add well over 10 MB. The RSS target (under 50 MB idle) stands.
22. **Tool count is twelve.** `unmark_stale` is its own tool rather than a flag, because separate
    names are clearer for a model reading tool descriptions; `publish_version` (rollback) gets a
    tool because the REST API has the endpoint and tools map 1:1 onto endpoints; `begin_upload` and
    `upload_chunk` carry large bundles. `set_visibility` accepts `unpublished` as a value so
    unpublish and republish need no extra tools.
23. **Natural keys externally, integers internally.** Artifacts are addressed by slug, versions by
    number, users by handle, keys by their public prefix. Tables use `INTEGER PRIMARY KEY`. No UUID
    dependency, simpler joins, and nothing in the API ever exposes a row id.

### Kept as written

Slug rules (plus the `--` ban), reserved hosts, the request path for serving, the five lifecycle
states and their transitions, the audit log, the two-host split for user and admin APIs, the security
checklist, the size and quota limits, the CLI with `--json`, soft delete with retention, blob GC.

---

## 2. Scope

### In v1 (Phases 0 to 3)

- Serve artifacts at `<slug>.<base>` and `<slug>--v<N>.<base>` over HTTPS with one wildcard cert
  (ACME DNS-01 via CertMagic, or cert files with hot reload), or plain HTTP behind a reverse proxy.
- Create, update, get, list, delete (soft and hard), publish a version (rollback), mark and unmark
  stale, set visibility (`public`, `unlisted`, `unpublished`), read a file from any version.
- Multi-file bundles, inline (JSON), streamed (multipart tar.gz) and chunked (staged) uploads,
  content addressing, optimistic concurrency with `expected_version`.
- Users and API keys, admin token, admin API, audit log, scheduler (sweep, retention, blob GC),
  Prometheus metrics, health endpoint, rate limits.
- MCP server (twelve tools, two resource templates, one prompt) with API-key auth, then OAuth 2.1
  with sign-in through the organisation's OIDC identity provider.
- `agalma` CLI client, Docker image, systemd unit, signed release binaries.

### Deferred (Phase 4)

Gated visibility with access keys, S3 blob backend, Postgres store, idle expiry, stale banner,
gallery at the apex, runtime-mutable settings, per-artifact custom apex domains, Markdown rendering,
per-artifact KV or SQLite capability, comments, public-instance hardening checklist, Enterprise
Managed Auth (RFC 7523 identity assertions), more than one identity provider per instance.

### Out of scope

Server-side functions or workers, multiplayer editing, connector-backed live data.

---

## 3. Design principles

These are the rules the code is reviewed against.

**Packages are named for what they provide, not for a layer.** No `models`, `services`, `utils`,
`handlers`. `artifact` holds the artifact domain and its service; `site` serves artifacts; `sqlite`
stores things in SQLite.

**Interfaces are defined by the consumer and kept small.** `artifact.Store` lives in `artifact`
because `artifact.Service` consumes it; `sqlite` implements it without importing `artifact`'s
internals beyond the types. An interface exists only where there is, or will plausibly be, a second
implementation: storage, blobs, certificate source, authenticator, audit recorder, clock.
Everything else is a concrete type. HTTP handlers take `*artifact.Service`, not an interface.

**Constructor injection, wired in one place.** Every package exports `New(deps..., Options)` and
nothing reads globals or config files. `internal/app` is the only package that knows every other
package; it builds the graph from `config.Config` and starts the servers. There is no DI container.

**Options structs with zero-value defaults.** `artifact.New(store, blobs, artifact.Options{})` works.
Functional options only if a constructor accumulates many optional knobs (none are expected).

**Time is injected.** Services that stamp or compare times hold `now func() time.Time` (default
`time.Now`), set via Options. The scheduler exposes `Trigger(name)` so tests never sleep.

**Errors carry a code.** `artifact.Error{Code, Message}` plus sentinels (`ErrNotFound`,
`ErrSlugTaken`, `ErrVersionConflict`, `ErrTooLarge`, `ErrInvalidSlug`, `ErrForbidden`). The HTTP
layer maps codes to status in one function; the MCP layer maps them to tool errors in one function.
Nothing below those layers knows about HTTP status codes.

**Stores are coarse-grained.** Each `Store` method is one unit of work and owns its transaction
(`Create` inserts artifact, version, files and actor row together). Transactions never leak into the
service, so a Postgres implementation has the same contract.

**Streams, not buffers.** Uploads flow `multipart -> tar -> counting limit reader -> sha256 -> temp
file -> rename` without ever holding a file in memory. Serving uses `http.ServeContent` on an
`io.ReadSeekCloser` from the blob store.

**Standard library first.** `net/http` (1.22 `ServeMux` host patterns do the host routing),
`log/slog`, `database/sql`, `archive/tar`, `compress/gzip`, `embed`, `testing`, `httptest`,
`crypto/*`, `mime`. Third-party only where the stdlib has nothing: MCP SDK, CertMagic, SQLite
driver, TOML, Prometheus client, `x/time/rate`, `x/sync/errgroup`, `go-cmp` in tests.

**Tests run against real components where they are cheap.** SQLite in a temp dir instead of a fake
store; a temp dir blob store; in-memory MCP transports; `httptest` with `req.Host` set. Hand-written
fakes only for things that are slow or external (ACME, clock). No mock generators, no assertion
library.

---

## 4. Package layout

```
cmd/agalma/                 main: subcommands serve | admin | publish | update | get | list | ...
internal/
  app/                      wiring and lifecycle: builds every component from config, runs servers,
                            graceful shutdown; end-to-end tests live here
  config/                   Config struct, Load (TOML file + AGALMA_* env), Validate, defaults
  api/                      wire types only: request/response structs and error codes shared by
                            httpapi, mcpserver and cli. No logic, no imports beyond stdlib
  artifact/                 domain: Artifact, Version, File, Visibility; Service (create, update,
                            publish, list, lifecycle, GC); slug/path/metadata validation; errors;
                            Store interface; storetest/ conformance suite for Store implementations
  blob/                     content-addressed blob Store interface; FS implementation; blobtest/
  upload/                   chunked upload staging: Begin, AppendChunk, Files (as artifact.FileInput),
                            Abort, ExpireStale; Store interface; staging files under data_dir/uploads
  sqlite/                   Open, embedded migrations, implementations of artifact.Store,
                            user.Store, audit.Store, oauth.Store
  user/                     User, APIKey; Service (create user, issue/revoke key, verify key);
                            Store interface; implements auth.Authenticator for API keys
  auth/                     Principal, context helpers, Authenticator interface, Chain,
                            RequireBearer middleware, RequireAdmin middleware, scope checks
  oauth/                    OAuth 2.1 authorization server (Phase 2b): metadata, DCR, CIMD,
                            authorize, upstream sign-in callback, consent page, token, refresh
                            rotation; IdentityProvider and Store interfaces; implements
                            auth.Authenticator for access tokens
  oidc/                     OpenID Connect relying party (Phase 2b): discovery, authorization code
                            flow with PKCE and nonce, ID token verification; implements
                            oauth.IdentityProvider over go-oidc
  httpapi/                  user REST API (api.<base>/v1) and admin REST API (admin.<base>/v1):
                            two constructors, shared JSON and error helpers, upload parsing
  mcpserver/                MCP tools, resources, prompt over *artifact.Service; Streamable HTTP
                            handler factory; principal from the request
  site/                     artifact serving: host parsing, Resolver interface, file serving,
                            headers (CSP, cache, robots), 404/410 pages
  certs/                    certificate Source: Files (hot reload), ACME (CertMagic + libdns),
                            SelfSigned (dev); builds *tls.Config
  scheduler/                named periodic jobs with jitter, Run(ctx), Trigger(name), status
  audit/                    Event, Recorder interface, Nop recorder
  middleware/               request ID, slog request logging, panic recovery, real IP in proxy mode
  cli/                      client commands over the user and admin APIs, --json, tar.gz from a dir
  buildinfo/                version, commit, date (ldflags with debug.ReadBuildInfo fallback)
docs/
  adr/                      architecture decision records
deploy/                     Dockerfile, agalma.service, example agalma.toml, example Caddyfile
```

**Dependency direction** (an arrow means "imports"):

```
cmd/agalma -> app -> {config, httpapi, mcpserver, site, certs, scheduler, sqlite, blob, ...}
httpapi    -> {api, artifact, upload, user, auth, audit, scheduler}
mcpserver  -> {api, artifact, upload, auth}
upload     -> {artifact (FileInput and ValidatePath)}
site       -> {artifact (types only), blob}   (artifact.Service satisfies site.Resolver)
artifact   -> {blob, audit}
user, oidc -> {auth}
oauth      -> {auth, user}                (user.Service resolves an auth.Identity to a user)
sqlite     -> {artifact, upload, user, audit, oauth}   (implements their Store interfaces)
cli        -> {api}
```

`artifact`, `user`, `auth`, `blob`, `audit`, `site`, `certs`, `scheduler`, `oidc` never import
`net/http` handlers, `sqlite`, or each other beyond what is listed. `api` imports nothing of ours.

---

## 5. Core types and interfaces

Sketches, not final code. Names matter more than field lists.

### artifact

```go
type Visibility string // "public" | "unlisted"   (gated added in Phase 4)

type Artifact struct {
    Slug, Name, Description string
    OwnerHandle             string
    Visibility              Visibility
    Metadata                map[string]string
    PublishedVersion        int          // number of the version being served
    StaleAt, ExpiresAt      *time.Time
    UnpublishedAt           *time.Time
    DeletedAt, PurgedAt     *time.Time
    CreatedAt, UpdatedAt    time.Time
}

type Version struct {
    Number    int
    Label     string
    CreatedBy string      // handle
    CreatedAt time.Time
    SizeBytes int64
    Files     []File      // sorted by Path
}

type File struct {
    Path        string    // normalised, no leading slash
    Hash        string    // hex sha256
    Size        int64
    ContentType string
}

type FileInput struct {
    Path        string
    Content     io.Reader // nil with Delete=true removes the path on update
    ContentType string    // optional; derived from extension, then sniffed
    Delete      bool
}

type Actor struct {
    Handle string
    Admin  bool
}

type CreateRequest struct {
    Slug, Name, Description string
    Visibility              Visibility
    Metadata                map[string]string
    Label                   string
    Files                   []FileInput
}

type UpdateRequest struct {
    Slug              string
    Name, Description *string
    Metadata          map[string]*string // nil value deletes the key
    Visibility        *Visibility
    Label             string
    ExpectedVersion   int                // 0 = no check
    KeepStale         bool
    Files             []FileInput        // empty = fields-only update, no new version
}

type Store interface {
    Create(ctx context.Context, a *Artifact, v *Version) error
    Get(ctx context.Context, slug string) (*Artifact, error)
    GetVersion(ctx context.Context, slug string, number int) (*Version, error)
    ListVersions(ctx context.Context, slug string) ([]Version, error)
    AddVersion(ctx context.Context, a *Artifact, v *Version, expected int) error
    Update(ctx context.Context, a *Artifact) error
    List(ctx context.Context, q ListQuery) (ListPage, error)
    ResolvePublished(ctx context.Context, slug string) (*Published, error)
    ResolveVersion(ctx context.Context, slug string, number int) (*Published, error)
    Files(ctx context.Context, versionID int64) (map[string]File, error)
    ListExpired(ctx context.Context, now time.Time) ([]Artifact, error)
    ListRetentionDue(ctx context.Context, now time.Time) ([]Artifact, error)
    ListTombstonesDue(ctx context.Context, now time.Time) ([]Artifact, error)
    Purge(ctx context.Context, slug string) error
    DeleteTombstone(ctx context.Context, slug string) error
    UnreferencedBlobs(ctx context.Context, limit int) ([]string, error)
}

type Options struct {
    MaxVersionBytes, MaxFileBytes int64
    MaxFiles                      int
    MaxMetadataPairs              int
    ReservedSlugs                 []string
    DefaultGrace, Retention       time.Duration
    Tombstone                     time.Duration
    WritePolicy                   string // "shared" | "owner"
    Now                           func() time.Time
}

func New(store Store, blobs blob.Store, rec audit.Recorder, opts Options) *Service

func (s *Service) Create(ctx, actor Actor, req CreateRequest) (*Artifact, *Version, error)
func (s *Service) Update(ctx, actor Actor, req UpdateRequest) (*Artifact, *Version, error)
func (s *Service) Get(ctx, actor Actor, slug string) (*Artifact, []Version, error)
func (s *Service) List(ctx, actor Actor, q ListQuery) (ListPage, error)
func (s *Service) Publish(ctx, actor Actor, slug string, version int) (*Artifact, error)
func (s *Service) MarkStale(ctx, actor Actor, slug string, grace time.Duration) (*Artifact, error)
func (s *Service) UnmarkStale(ctx, actor Actor, slug string) (*Artifact, error)
func (s *Service) SetVisibility(ctx, actor Actor, slug string, v string) (*Artifact, error)
func (s *Service) Delete(ctx, actor Actor, slug string, hard bool) error
func (s *Service) OpenFile(ctx, actor Actor, slug, path string, version int) (File, io.ReadSeekCloser, error)

// Serving path (satisfies site.Resolver); no actor, visibility applied by caller.
func (s *Service) ResolvePublished(ctx, slug string) (*Published, error)
func (s *Service) ResolveVersion(ctx, slug string, n int) (*Published, error)
func (s *Service) Files(ctx, versionID int64) (map[string]File, error)

// Scheduler jobs.
func (s *Service) Sweep(ctx) (SweepReport, error)        // stale -> soft-deleted, soft-deleted -> purged, tombstones -> freed
func (s *Service) CollectGarbage(ctx) (int, error)        // unreferenced blobs -> deleted
```

`Published` carries what the serving path needs and nothing more: slug, version number, version id,
visibility, `unpublished_at`, `deleted_at`, `purged_at`, and the `agalma.*` keys from metadata
already parsed into typed fields (`SPA bool`, `Indexable bool`, `CSP string`).

Validation lives in `artifact` as plain functions with table-driven tests: `ValidateSlug`,
`ValidatePath`, `ValidateMetadata`. The service calls them; the HTTP and MCP layers do not
duplicate them.

### blob

```go
type Store interface {
    Put(ctx context.Context, r io.Reader) (hash string, size int64, err error) // idempotent
    Open(ctx context.Context, hash string) (io.ReadSeekCloser, error)
    Stat(ctx context.Context, hash string) (size int64, err error)
    Delete(ctx context.Context, hash string) error
}
```

`blob.FS` stores `blobs/<hash[0:2]>/<hash>`; `Put` writes to a temp file in the same directory,
hashes while writing, then renames (no partial blobs are ever visible). Size limits are enforced by
the caller (`artifact.Service`) with a counting reader that fails fast, so `blob` stays dumb and an
S3 implementation needs no limit logic.

### upload

```go
type Upload struct {
    ID        string    // random, URL-safe
    Owner     string    // handle
    Bytes     int64     // staged so far, all files
    ExpiresAt time.Time
    Files     []StagedFile
}

type StagedFile struct {
    Path        string
    Bytes       int64
    ContentType string
}

type Store interface {
    Create(ctx context.Context, u *Upload) error
    Get(ctx context.Context, id string) (*Upload, error)
    UpsertFile(ctx context.Context, id string, f StagedFile, totalBytes int64) error
    Delete(ctx context.Context, id string) error
    ListExpired(ctx context.Context, now time.Time) ([]Upload, error)
    CountOpen(ctx context.Context, owner string) (int, error)
}

type Options struct {
    Dir              string        // data_dir/uploads
    MaxChunkBytes    int64         // default 4 MiB, after base64 decoding
    MaxTotalBytes    int64         // same as limits.max_version_bytes
    TTL              time.Duration // default 1h
    MaxOpenPerOwner  int           // default 5
    Now              func() time.Time
}

func New(store Store, opts Options) *Service

func (s *Service) Begin(ctx, owner string) (*Upload, error)
func (s *Service) AppendChunk(ctx, owner, id, path string, offset int64, r io.Reader, contentType string) (StagedFile, error)
func (s *Service) Files(ctx, owner, id string) ([]artifact.FileInput, func() error, error) // readers + cleanup
func (s *Service) Abort(ctx, owner, id string) error
func (s *Service) ExpireStale(ctx) (int, error) // scheduler job
```

Chunks append to a staging file per path under `uploads/<id>/<n>` (an index, never the user's path).
`offset` makes retries safe: a chunk whose `offset + len` equals the current size is acknowledged
without writing; any other mismatch returns `upload_offset_mismatch` with the current size so the
client resumes from there. Paths go through `artifact.ValidatePath` at the first chunk. `Files`
hands the staged files to `artifact.Service.Create` or `Update` as ordinary `FileInput`s, so the
artifact code never knows the bytes arrived in pieces; the cleanup function removes the staging
directory and rows after the publish succeeds or fails. The scheduler removes uploads past their
TTL.

### site

```go
type Resolver interface {
    ResolvePublished(ctx context.Context, slug string) (*artifact.Published, error)
    ResolveVersion(ctx context.Context, slug string, n int) (*artifact.Published, error)
    Files(ctx context.Context, versionID int64) (map[string]artifact.File, error)
}

type Options struct {
    BaseDomain      string
    DefaultCSP      string
    AllowCSPOverride bool
    NotFoundPage    []byte // instance default 404
}

func NewHandler(r Resolver, blobs blob.Store, opts Options) http.Handler
func ParseHost(host, base string) (slug string, version int, ok bool)
```

### auth

```go
type Principal struct {
    Handle string
    Admin  bool
    Scopes []string
    Source string // "api_key" | "oauth" | "admin_token"
    KeyID  string // for audit and rate limiting
}

type Authenticator interface {
    Authenticate(ctx context.Context, token string) (Principal, error) // ErrInvalidToken
}

// Identity is what an upstream identity provider asserts about a person.
type Identity struct {
    Issuer, Subject string // stable key
    Email           string
    EmailVerified   bool
    Name            string
}

func Chain(auths ...Authenticator) Authenticator
func RequireBearer(a Authenticator, opts BearerOptions) func(http.Handler) http.Handler
func RequireScope(scope string) func(http.Handler) http.Handler
func RequireAdminToken(token string) func(http.Handler) http.Handler
func FromContext(ctx context.Context) (Principal, bool)
```

`BearerOptions.ResourceMetadataURL`, when set (the MCP host), makes a `401` carry
`WWW-Authenticate: Bearer resource_metadata="..."` as RFC 9728 and Claude's client require.

### oauth and oidc

```go
// oauth.IdentityProvider sends a browser to sign in and turns the callback into an Identity.
type IdentityProvider interface {
    AuthURL(state, nonce, codeChallenge string) string
    Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error)
}

func oidc.New(ctx context.Context, cfg oidc.Config) (*oidc.Provider, error) // discovery at startup
```

`user.Service.ResolveIdentity(ctx, id auth.Identity) (User, error)` applies the provisioning policy:
match on `(issuer, subject)`; else link by verified email to an existing user; else create a member
when `auto_provision` is on and the email domain is allow-listed; else `ErrNotProvisioned`.

### certs

```go
type Source interface {
    GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
    Run(ctx context.Context) error // renewal loop or file watcher; returns on ctx done
}

func NewFiles(certFile, keyFile string, pollEvery time.Duration) (*Files, error)
func NewACME(cfg ACMEConfig, dns libdns.Provider, storageDir string) (*ACME, error)
func NewSelfSigned(domains ...string) (*SelfSigned, error)
func TLSConfig(s Source) *tls.Config
```

### scheduler

```go
type Job struct {
    Name  string
    Every time.Duration
    Run   func(ctx context.Context) error
}

func New(logger *slog.Logger, jobs ...Job) *Scheduler
func (s *Scheduler) Run(ctx context.Context) error     // jittered tickers, one job at a time per name
func (s *Scheduler) Trigger(ctx context.Context, name string) error
func (s *Scheduler) Status() []JobStatus               // last run, last error, next run
```

### audit

```go
type Event struct {
    At      time.Time
    Actor   string // handle or "scheduler" / "admin"
    Action  string // artifact.create, artifact.update, artifact.stale, ...
    Slug    string
    Details map[string]string
}

type Recorder interface{ Record(ctx context.Context, e Event) error }
type Reader interface{ List(ctx context.Context, q Query) ([]Event, error) } // admin API
```

Audit rows are written after the store call returns, outside its transaction. A crash in that
window loses one audit row. This is accepted for v1 and documented here rather than coupling the
artifact store to the audit table.

---

## 6. Request flows

**Publish (REST, multipart).** `httpapi` reads the `manifest` part (JSON: name, description,
metadata, label, expected_version, deletes), then opens the `bundle` part as `gzip -> tar` and
yields one `FileInput` per regular entry with the tar reader as `Content`. `artifact.Service.Update`
validates each path, wraps the reader in a per-file and per-version limit, derives the content type,
streams it into `blob.Put`, and collects `File` rows. It then builds the new version from the
previous version's files plus the additions and deletions, and calls `store.AddVersion` with the
expected version. Finally it records an audit event and returns. The JSON body form does the same
with inline strings (`encoding: utf8|base64`) decoded into readers. Create is the same without a
previous version.

**Publish (MCP).** The tool handler converts its typed input into the same `CreateRequest` or
`UpdateRequest` and calls the same service method. Results go back as structured content using
`api` types plus a one-line text summary containing the URL.

**Serve.** `ServeMux` host patterns send `api.`, `mcp.`, `admin.` and the apex to their handlers.
The host-less `/` pattern catches every other host and hands it to `site`. `site.ParseHost` strips
the port and base domain and splits `--v<N>`. The handler resolves the slug (published or pinned
version), returns 404 for unknown, unpublished or soft-deleted, 410 for purged, then loads the
version's file map, applies the path rules (directory to `index.html`, miss to `404.html`, SPA
fallback), opens the blob and calls `http.ServeContent` with the ETag set to the content hash.
Headers: CSP, `Cache-Control` (`no-cache` on the latest host, `public, max-age=31536000, immutable`
on version hosts), `X-Robots-Tag`, `X-Content-Type-Options: nosniff`.

**Scheduler.** `app` registers `sweep` (every 15 min), `gc` (hourly) and `uploads-expire` (every
10 min) jobs that call `artifact.Service.Sweep`, `CollectGarbage` and `upload.Service.ExpireStale`;
`certs.ACME.Run` manages renewal itself. The admin API exposes `scheduler.Status()` and `Trigger`.

**Publish (chunked).** `begin_upload` creates a staging record and returns its id, expiry and the
chunk limit. Each `upload_chunk` call appends one piece of one file at a stated offset. Finally
`create_artifact` or `update_artifact` is called with `upload_id`; the handler asks
`upload.Service.Files` for the staged files as `FileInput`s, merges any inline `files[]` from the
same call (small files and `delete` entries), and runs the normal publish. Success or failure, the
staging directory is removed afterwards.

---

## 7. Data model

SQLite, WAL mode, foreign keys on. Migrations are numbered SQL files embedded in `internal/sqlite`
and applied at startup by a 60-line migrator with a `schema_migration` table. Queries are
hand-written `database/sql`; keep them ANSI where possible so a Postgres store can reuse most of
them.

| Table            | Columns (key ones)                                                                                                                                                                    | Notes                                                                                   |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `user`           | id, handle UNIQUE, email UNIQUE NULL, display_name, role (`admin`/`member`), quota_bytes, disabled_at, created_at                                                                     | Created by the admin API or provisioned on first OIDC sign-in from an allowed domain    |
| `user_identity`  | user_id, issuer, subject, email_at_login, last_login_at; UNIQUE(issuer, subject)                                                                                                      | Phase 2b; links an upstream identity to a user                                          |
| `api_key`        | id, user_id, prefix UNIQUE, hash, label, scopes, last_used_at, expires_at, revoked_at, created_at                                                                                     | Key shown once as `agk_<prefix>_<secret>`; SHA-256 of the whole key stored               |
| `artifact`       | id, slug UNIQUE, name, description, owner_id, visibility, published_version_id, metadata (JSON text), stale_at, expires_at, unpublished_at, deleted_at, purged_at, created_at, updated_at | `purged_at` set means tombstone; row deleted after the tombstone period                 |
| `version`        | id, artifact_id, number, label, created_by, created_at, size_bytes; UNIQUE(artifact_id, number)                                                                                       | Immutable                                                                               |
| `version_file`   | version_id, path, blob_hash, size, content_type; PRIMARY KEY(version_id, path)                                                                                                        | Replaces the JSON manifest                                                              |
| `blob`           | hash PRIMARY KEY, size, created_at                                                                                                                                                    | GC: `blob LEFT JOIN version_file ... WHERE version_file.blob_hash IS NULL`               |
| `upload`         | id (string) PRIMARY KEY, owner_id, bytes, created_at, expires_at                                                                                                                      | Phase 2a; a chunked upload in progress                                                  |
| `upload_file`    | upload_id, path, staging_index, bytes, content_type; PRIMARY KEY(upload_id, path)                                                                                                     | Phase 2a; one staged file                                                               |
| `artifact_actor` | artifact_id, user_id, last_action, last_at; PRIMARY KEY(artifact_id, user_id)                                                                                                         | Powers `mine=created|updated|any`                                                       |
| `audit_event`    | id, at, actor, action, slug, details (JSON text)                                                                                                                                      | Append only                                                                             |
| `oauth_client`   | id (string), name, redirect_uris (JSON), created_at                                                                                                                                   | Phase 2b; DCR registrations and cached CIMD documents                                   |
| `oauth_request`  | id, client_id, redirect_uri, scopes, code_challenge, resource, client_state, nonce, upstream_verifier, expires_at                                                                       | Phase 2b; a pending authorize request while the browser is at the identity provider     |
| `oauth_code`     | code_hash, client_id, user_id, scopes, redirect_uri, pkce_challenge, resource, expires_at                                                                                             | Phase 2b; single use                                                                    |
| `oauth_token`    | id, token_hash UNIQUE, kind (`access`/`refresh`), client_id, user_id, scopes, expires_at, revoked_at, replaced_by                                                                      | Phase 2b; opaque tokens, rotation recorded via `replaced_by`                            |

Indexes: `artifact(owner_id)`, `artifact(expires_at)`, `artifact(deleted_at)`, `version_file(blob_hash)`,
`artifact_actor(user_id, last_at)`, `audit_event(at)`.

Metadata rules: keys `[a-zA-Z0-9_.-]{1,64}`, values up to 4 KiB, at most 64 pairs. Keys under
`agalma.` are server-interpreted and limited to a known set (`agalma.spa`, `agalma.indexable`,
`agalma.csp`); unknown `agalma.*` keys are rejected. Everything else is opaque and filterable with
`metadata.<key>=<value>` in list calls (`json_extract(metadata, '$.' || ?) = ?` with the key
validated against the rule above before it is interpolated).

---

## 8. HTTP and MCP surfaces

### User API, `https://api.<base>/v1`, bearer API key or OAuth access token

| Method | Path                                   | Purpose                                                                                |
| ------ | -------------------------------------- | -------------------------------------------------------------------------------------- |
| POST   | `/artifacts`                           | Create. JSON (inline files and/or `upload_id`) or multipart (`manifest` + `bundle` tar.gz) |
| GET    | `/artifacts`                           | List: `mine=created|updated|any`, `metadata.<k>=<v>`, `stale=include|exclude|only`, `cursor`, `limit` |
| GET    | `/artifacts/{slug}`                    | Record plus versions (file lists on request: `?files=true`)                             |
| PUT    | `/artifacts/{slug}`                    | Update files and/or fields; `If-Match: "<version>"` or `expected_version` in the manifest |
| PATCH  | `/artifacts/{slug}`                    | Fields only: name, description, metadata (merge; null deletes), visibility              |
| POST   | `/uploads`                             | Begin a chunked upload; returns id, expiry, chunk limit                                 |
| PATCH  | `/uploads/{id}/files/{path...}`        | Append a raw-body chunk at `?offset=`; `Content-Type` sets the file's type on first chunk |
| DELETE | `/uploads/{id}`                        | Abort and discard                                                                       |
| POST   | `/artifacts/{slug}/publish`            | `{ "version": 3 }` moves the published pointer (rollback)                                |
| POST   | `/artifacts/{slug}/stale`              | Mark stale, optional `{ "grace": "336h" }`                                              |
| DELETE | `/artifacts/{slug}/stale`              | Unmark stale                                                                            |
| GET    | `/artifacts/{slug}/files/{path...}`    | Read a file; `?version=N`                                                               |
| DELETE | `/artifacts/{slug}`                    | Soft delete; `?hard=true` needs `artifacts:delete`                                      |
| GET    | `/me`                                  | Caller identity and scopes                                                              |
| GET    | `/me/keys`                             | Own keys (prefix, label, last used)                                                     |

Errors are `{ "error": { "code": "slug_taken", "message": "..." } }` with the matching status
(404, 409, 412 for version conflict, 413, 422, 403, 401, 429).

### Admin API, `https://admin.<base>/v1`, admin token from config or an admin-role key

| Method           | Path                        | Purpose                                                       |
| ---------------- | --------------------------- | ------------------------------------------------------------- |
| GET/POST         | `/users`                    | List, create (`handle`, `role`, `quota_bytes`)                |
| PATCH            | `/users/{handle}`           | Disable, change role or quota                                 |
| GET/POST         | `/users/{handle}/keys`      | List, issue (`label`, `scopes`, `expires_in`); key returned once |
| DELETE           | `/users/{handle}/keys/{prefix}` | Revoke                                                    |
| GET              | `/artifacts`                | Every artifact, any owner, with size and state                |
| POST             | `/artifacts/{slug}/transfer`| `{ "owner": "handle" }`                                       |
| POST             | `/artifacts/{slug}/hide`    | Unpublish in one call (abuse response)                        |
| GET              | `/settings`                 | Effective config with secrets redacted                        |
| GET              | `/jobs`, POST `/jobs/{name}/run` | Scheduler status and manual trigger                     |
| GET              | `/audit?since=&slug=&limit=`| Audit log                                                     |
| GET              | `/health`, `/metrics`       | Liveness (also on the apex), Prometheus                       |

The admin host can also bind a second listener (`admin.listen`, for example `127.0.0.1:9443`) so an
operator can keep it off the public interface entirely.

### MCP, `https://mcp.<base>/`, Streamable HTTP, stateless

| Tool               | Input                                                                              | Returns                               |
| ------------------ | ---------------------------------------------------------------------------------- | ------------------------------------- |
| `create_artifact`  | slug, name, description, metadata?, visibility?, label?, files[]? `{path, content, encoding?, content_type?}`, upload_id? | url, version, artifact |
| `update_artifact`  | slug, files[]? (with `delete: true` entries), upload_id?, name?, description?, metadata? (null deletes), visibility?, label?, expected_version?, keep_stale? | url, version, artifact |
| `begin_upload`     | (none)                                                                             | upload_id, expires_at, max_chunk_bytes, max_total_bytes |
| `upload_chunk`     | upload_id, path, offset, content, encoding?, content_type?                         | path, bytes_received                  |
| `get_artifact`     | slug, include_files?                                                               | artifact, versions                    |
| `read_file`        | slug, path, version?                                                               | text inline; small binaries base64; else API URL |
| `list_artifacts`   | mine? (`created|updated|any`), metadata?, stale?, cursor?, limit?                  | page                                  |
| `publish_version`  | slug, version                                                                      | artifact                              |
| `mark_stale`       | slug, grace?                                                                       | expires_at                            |
| `unmark_stale`     | slug                                                                               | artifact                              |
| `set_visibility`   | slug, visibility (`public|unlisted|unpublished`)                                   | url, artifact                         |
| `delete_artifact`  | slug, hard?                                                                        | ok                                    |

Tool descriptions state the slug rules, the size limits, the CSP, and what the URL will be, because
a model reads nothing else. `create_artifact` and `update_artifact` say in their description when to
switch to `begin_upload` (any single file or bundle over the inline guidance, default 2 MiB), and
`upload_chunk`'s description states the chunk cap and the offset rule, so a model can recover from a
dropped call without reading documentation. Resources: `agalma://artifacts/{slug}` and
`agalma://artifacts/{slug}/files/{path}`. Prompt: `publish-page`, which carries the constraints and
the conventions an author needs (single `index.html`, relative links, which CDNs the CSP allows).
A `SKILL.md` for Claude Code ships alongside and says the same things.

Tool handlers are closures over the request's `auth.Principal`: the Streamable HTTP handler is built
with a per-request server factory (`func(*http.Request) *mcp.Server`) that reads the principal
`auth.RequireBearer` placed in the context. Per-request server construction is the SDK's documented
stateless pattern and costs microseconds for twelve tools.

---

## 9. Authentication

**API keys.** Format `agk_<8-char prefix>_<32 random bytes base32>`. The prefix is stored in clear
for lookup and display; the SHA-256 of the whole key is compared in constant time. Scopes:
`artifacts:read`, `artifacts:write`, `artifacts:delete`, `admin`. `last_used_at` is written at most
once per minute per key.

**Admin token.** A single string in config (or `AGALMA_ADMIN_TOKEN`). Accepted only on the admin
host. Admin-role API keys are also accepted there, so the config token can be rotated out once a
real admin user exists.

**OAuth 2.1 authorization server (Phase 2b).** Issuer is `https://mcp.<base>` so one `/.well-known/`
set serves both the protected-resource and authorization-server metadata:

- `GET /.well-known/oauth-protected-resource`: `resource` = the MCP URL exactly as entered,
  `authorization_servers` = the issuer, `scopes_supported`.
- `GET /.well-known/oauth-authorization-server`: endpoints, `code_challenge_methods_supported:
  ["S256"]`, `client_id_metadata_document_supported: true`, `token_endpoint_auth_methods_supported:
  ["none"]`, `grant_types_supported: ["authorization_code","refresh_token"]`.
- `POST /oauth/register` (DCR, JSON): stores a public client with its redirect URIs.
- CIMD: a `client_id` that is an `https` URL is fetched (10 s timeout, cached, size-capped) and its
  `redirect_uris` used. Claude Code's loopback URIs (`http://localhost/callback`,
  `http://127.0.0.1/callback`) are matched with the port ignored; every other redirect URI must
  match exactly.
- `GET /oauth/authorize`: validates client, redirect URI, PKCE (S256 required) and the `resource`
  parameter, stores the pending request as an `oauth_request` row with a ten-minute expiry, sets a
  cookie holding that row's id (`Secure; HttpOnly; SameSite=Lax`, scoped to the `mcp.` host), and
  redirects the browser to the identity provider with its own `state`, `nonce` and PKCE pair.
- `GET /oauth/callback`: exchanges the upstream code, verifies the ID token (signature against the
  provider's JWKS, issuer, audience, nonce, expiry), builds an `auth.Identity`, resolves it to a
  user through `user.Service.ResolveIdentity`, and shows a one-click consent page (plain HTML from
  `embed`, no JavaScript) naming the client and the scopes. Approval issues a code bound to that
  user and to the intersection of the requested scopes and the scopes the user's role allows
  (members: `artifacts:read artifacts:write artifacts:delete`; admins add `admin`). A person the
  policy does not admit sees a page telling them to ask their administrator, and nothing is issued.
- No identity provider configured: the authorize endpoint renders the consent page directly with an
  API-key field instead of redirecting; the grant is bound to that key's user and capped at the
  key's scopes. This keeps a one-person instance usable with zero identity setup.
- No separate "remember me" session in v1: every authorize request goes through the provider,
  whose own SSO session makes the round trip silent. MCP clients hold refresh tokens, so the flow
  runs once per connection, not per call.
- `POST /oauth/token` (form-urlencoded): `authorization_code` with `code_verifier`;
  `refresh_token` with rotation (old token marked `replaced_by`). Access tokens live one hour,
  refresh tokens thirty days; both opaque, stored hashed. Errors use RFC 6749 codes
  (`invalid_grant` for a dead refresh token, as Claude's client expects).
- Unauthenticated MCP requests get `401` with `WWW-Authenticate: Bearer resource_metadata="..."`.

`oauth.Service` implements `auth.Authenticator`; `app` chains it with the API-key authenticator so
every surface accepts either credential.

**Identity providers (Phase 2b).** One OIDC provider per instance, configured under `[auth.oidc]`
and discovered at startup from `<issuer>/.well-known/openid-configuration`. The redirect URI to
register at the provider is `https://mcp.<base>/oauth/callback`. Requested scopes are
`openid email profile`. Provider notes:

- **Microsoft Entra ID** (the first provider tested live): register a single-tenant web app; issuer is
  `https://login.microsoftonline.com/<tenant-id>/v2.0`. The multi-tenant `common` endpoint is not
  supported in v1 because its issuer varies per tenant and defeats strict issuer checking. Entra
  omits `email` unless it is added as an optional claim, so `preferred_username` is the fallback.
- **Google Workspace**: issuer `https://accounts.google.com`; `email_verified` is honoured and the
  `hd` claim must match an allowed domain when one is configured.
- **Okta, Keycloak, Authentik, Zitadel, Auth0**: standard discovery; nothing provider-specific.

Provisioning policy, all in config: `allowed_domains` (verified email domains that may sign in),
`auto_provision` (create a member on first sign-in from an allowed domain; off means only
admin-created users with a matching email can sign in), `admin_emails` (provisioned as admins).
Handles for provisioned users derive from the email local part and are made unique with a numeric
suffix. Admins can also create users with an email ahead of time, link or unlink identities, and
disable a user, which revokes every token and key.

**Enterprise Managed Auth (Phase 4).** Claude's silent SSO path sends a signed identity assertion
from the customer's identity provider to the token endpoint using the RFC 7523 JWT bearer grant
(`urn:ietf:params:oauth:grant-type:jwt-bearer`). It is available on Claude Team and Enterprise
plans, needs the client pre-registered (CIMD or Anthropic-held credentials, not DCR), a per-tenant
allow-list of trusted issuers verified against their JWKS, and the grant advertised in
`grant_types_supported`. Okta's cross-app-access playground can exercise it without a Claude
organisation. The 2b design does not block it: tokens, clients and the user model are the same.

---

## 10. Configuration

One TOML file, every key overridable by `AGALMA_<SECTION>_<KEY>` (for example
`AGALMA_TLS_DNS_API_TOKEN`), validated at startup with errors that name the key.

```toml
base_domain = "art.example.com"
data_dir    = "/var/lib/agalma"      # agalma.db, blobs/, certmagic/

[listen]
https      = ":443"
http       = ":80"                   # redirects to https; "" disables
proxy_mode = false                   # true: plain HTTP on [listen].http, trust X-Forwarded-*

[tls]
mode         = "acme"                # acme | files | self-signed
email        = "ops@example.com"
ca           = "letsencrypt"         # letsencrypt | letsencrypt-staging | <directory URL>
dns_provider = "cloudflare"          # cloudflare | route53 | hetzner
cert_file    = ""                    # mode = files
key_file     = ""
[tls.dns]
api_token = ""                       # provider-specific; prefer the env override

[limits]
max_version_bytes      = "50MiB"
max_file_bytes         = "25MiB"
max_files              = 2000
metadata_max_pairs     = 64
write_rate_per_minute  = 60
default_user_quota     = "2GiB"
inline_guidance_bytes  = "2MiB"      # quoted in tool descriptions as the point to switch to chunks
max_chunk_bytes        = "4MiB"      # per upload_chunk call, after base64 decoding
upload_ttl             = "1h"
max_open_uploads       = 5           # per user

[lifecycle]
default_grace  = "336h"              # 14 days
retention      = "720h"              # 30 days
tombstone      = "2160h"             # 90 days
sweep_interval = "15m"
gc_interval    = "1h"

[serve]
csp = "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: https:; connect-src 'self'; frame-ancestors 'none'"
allow_csp_override = true
reserved_slugs     = ["api", "mcp", "admin", "www", "mail", "ns1", "ns2", "autoconfig", "autodiscover", "localhost"]

[auth]
write_policy = "shared"              # shared | owner
admin_token  = ""                    # prefer AGALMA_AUTH_ADMIN_TOKEN

[auth.oidc]                          # omit the whole table to fall back to API-key sign-in
issuer          = "https://login.microsoftonline.com/<tenant-id>/v2.0"   # or https://accounts.google.com
client_id       = ""
client_secret   = ""                 # prefer AGALMA_AUTH_OIDC_CLIENT_SECRET
allowed_domains = ["example.com"]    # verified email domains that may sign in
auto_provision  = true               # create a member on first sign-in from an allowed domain
admin_emails    = []                 # provisioned with the admin role

[admin]
listen = ""                          # optional second listener for the admin host, e.g. "127.0.0.1:9443"

[log]
level  = "info"
format = "text"                      # text | json
```

Development runs with `base_domain = "localhost"` and either `tls.mode = "self-signed"` or
`proxy_mode = true`; browsers resolve `*.localhost` to loopback, so `http://demo.localhost:8080`
works with no DNS.

---

## 11. Serving semantics

- Slug: `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, 3 to 63 chars, no `--`, not in `reserved_slugs`.
  Immutable after create.
- Host parsing: lower-case, strip port, require the base-domain suffix, split on the last `--v`
  followed by digits. Anything else is a 404 with the instance page (never a redirect, never a
  different status for "exists but unpublished").
- Path: `path.Clean`, reject `..`, absolute, NUL, control characters, segments over 255 bytes,
  totals over 1024 bytes, more than `max_files` entries. A request for a directory serves
  `index.html` under it; a miss serves the artifact's own `404.html` if present, else the instance
  page; `agalma.spa = "true"` serves `index.html` for any miss without an extension.
- Content type: from the manifest if given, else `mime.TypeByExtension`, else sniffed from the first
  512 bytes at upload. Never from the request. Always `X-Content-Type-Options: nosniff`.
- Caching: `ETag: "<sha256>"` everywhere. Latest host: `Cache-Control: no-cache` (revalidate every
  time; cheap with a strong ETag). Version host: `public, max-age=31536000, immutable`.
- Visibility and state: `public` and `unlisted` both serve; `unpublished_at`, `deleted_at` give 404;
  `purged_at` gives 410 until the tombstone row is removed. `X-Robots-Tag: noindex` unless
  `agalma.indexable = "true"`.
- Control-plane hosts set `Cross-Origin-Opener-Policy: same-origin` and never set cookies with a
  `Domain` attribute. The only cookie in the system is the OAuth consent session, scoped to the
  `mcp.` host, `Secure; HttpOnly; SameSite=Lax`, ten-minute lifetime.

---

## 12. Lifecycle

States and transitions, as in the source diagram: Published, Unpublished, Stale (grace running),
Soft-deleted, Purged (blobs gone).

- `mark_stale` sets `stale_at = now`, `expires_at = now + grace` (default from config, per-call
  override, capped by a per-user maximum the admin can set). The page keeps serving.
- `unmark_stale` clears both. An `update_artifact` that adds a version also clears them unless
  `keep_stale` is set.
- `set_visibility unpublished` sets `unpublished_at`; `public` or `unlisted` clears it. Reversible at
  any time before purge.
- `delete_artifact` sets `deleted_at`; the slug stays reserved; an admin or the owner can restore
  through `PATCH` until retention elapses.
- `Sweep`: stale past `expires_at` becomes soft-deleted; soft-deleted past `retention` is purged
  (versions and files deleted, `purged_at` set); tombstones past `tombstone` are removed, freeing the
  slug. Each transition writes an audit event.
- `CollectGarbage`: blobs with no `version_file` reference are deleted from the blob store and the
  `blob` table, in batches, with the DB row removed only after the file is gone.

Every list and get response carries `stale_at`, `expires_at`, `unpublished_at`, `deleted_at` so an
agent can tell its user what is about to vanish.

---

## 13. Security checklist

Carried from the source plan and made concrete for the code:

- Origin split enforced by the host router; `site` never handles a reserved host, and the control
  plane never handles a wildcard host.
- Path normalisation and limits in `artifact.ValidatePath`, tested with the usual traversal and
  encoding cases. Tar entries that are not regular files are skipped; absolute and `..` names
  rejected.
- Upload limits: per file, per version, per user quota, file count; `http.MaxBytesReader` on every
  body; gzip and tar are decoded through a counting reader that aborts at the cap.
- Keys and tokens hashed with SHA-256; comparisons through `crypto/subtle`; keys shown exactly once.
- MCP endpoint checks `Origin` (must be absent or match an allow-list) and rejects unknown
  `MCP-Protocol-Version` values, as the spec requires against DNS rebinding.
- Rate limits: per key for writes (`x/time/rate`), per IP for unauthenticated requests on the
  control plane; `429` with `Retry-After`.
- `X-Robots-Tag: noindex` default, `frame-ancestors 'none'` in the default CSP, admin hide endpoint.
- Builds with `-trimpath`, `CGO_ENABLED=0`; releases carry an SBOM and cosign signatures (Phase 3).
- A security review of `site` and `artifact` validation before v1.0, with a written threat model in
  `docs/security.md`.

---

## 14. Testing strategy

| Layer                    | How                                                                                                                                      |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| Validation               | Table-driven unit tests in `artifact` (slugs, paths, metadata, host parsing in `site`)                                                   |
| `artifact.Service`       | Real `sqlite` store in `t.TempDir()`, real `blob.FS`, `audit` fake that records events, injected clock. Covers create/update/concurrency/limits/lifecycle |
| Store conformance        | `artifact/storetest.Run(t, func(t) artifact.Store)` exercised by `sqlite`; a future Postgres store runs the same suite                      |
| Blob conformance         | `blob/blobtest.Run` for `FS` (and later S3)                                                                                              |
| `upload`                 | Append, retry at the same offset, mismatch, chunk and total caps, TTL expiry with a fast clock, `Files` output as `FileInput`s, cleanup   |
| `site`                   | `httptest` with `req.Host = "demo.art.test"`; a resolver fake for state cases (404/410/SPA/headers), plus one run against the real service |
| `httpapi`                | `httptest.NewServer` over real service + sqlite + blobs; request bodies built with `mime/multipart` and `archive/tar`                     |
| `mcpserver`              | `mcp.NewInMemoryTransports()` client and server in one process; every tool round-trips; one test over real HTTP for the auth `401`        |
| `oauth`                  | Full code flow in-process with `httptest`: DCR, CIMD (served by a second test server), PKCE, refresh rotation, loopback matching, API-key fallback |
| `oidc`                   | A fake provider in `httptest` serving discovery, JWKS and token endpoints and signing ID tokens with a test key; covers nonce, audience, issuer and expiry failures; provisioning policy cases in `user` |
| `certs`                  | `Files` reload via rewritten temp files; `SelfSigned` handshake with `tls.Dial`; ACME covered by a CertMagic fake issuer and a staging-CA integration test behind a build tag |
| `scheduler`              | Jobs as counters, `Trigger` instead of waiting, context cancellation                                                                      |
| End to end (`app`)       | Boot the whole app in proxy mode on a random port with a temp data dir; publish through the REST API, update through MCP, fetch the page, mark stale, run the sweep with a fast clock, assert 404 then 410 |
| CI                       | `go vet`, `staticcheck`, `go test -race ./...`, `govulncheck`, build for linux/amd64, linux/arm64, darwin/arm64                              |

Rule: a bug fix adds a test at the lowest layer that reproduces it.

---

## 15. Dependencies

| Need                      | Choice                                                                | Notes                                                                                       |
| ------------------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| MCP                       | `github.com/modelcontextprotocol/go-sdk` v1.8+                         | Official SDK; Streamable HTTP, stateless mode, `auth.RequireBearerToken`, in-memory transports |
| TLS / ACME                | `github.com/caddyserver/certmagic` v0.25.6+, `github.com/libdns/{cloudflare,route53,hetzner}` | DNS-01 wildcard, storage, renewal. Add providers by import                            |
| SQLite                    | `github.com/ncruces/go-sqlite3` (`driver` + `embed`)                   | CGO-free, tracks upstream SQLite closely, `database/sql`; `modernc.org/sqlite` is a one-line swap |
| Config                    | `github.com/BurntSushi/toml`                                          |                                                                                             |
| OIDC relying party        | `github.com/coreos/go-oidc/v3` v3.18+, `golang.org/x/oauth2`           | Discovery, JWKS caching, ID token verification; Phase 2b                                    |
| Rate limiting             | `golang.org/x/time/rate`                                              |                                                                                             |
| Concurrency               | `golang.org/x/sync/errgroup`                                          | Server lifecycle in `app`                                                                   |
| Metrics                   | `github.com/prometheus/client_golang`                                 | Phase 3                                                                                     |
| Tests                     | `github.com/google/go-cmp`                                            | Diffs only; no assertion framework                                                          |

Not used, deliberately: cobra (stdlib `flag` with a subcommand switch), chi/gin/echo (`ServeMux`),
gorm/sqlx/sqlc (hand-written SQL, revisit sqlc if query count passes roughly sixty), testify,
mockery, wire/fx, uuid (natural keys), cron libraries, zap/logrus (`slog`).

---

## 16. Build, release, deploy

- `Makefile` or `Taskfile` targets: `build`, `test`, `lint`, `run-dev` (self-signed on
  `localhost`), `release` (goreleaser).
- `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ...buildinfo.version=..."`.
- goreleaser builds linux/amd64, linux/arm64, darwin/arm64; attaches SBOM (syft) and cosign
  signatures; publishes a distroless Docker image `ghcr.io/peterbooker/agalma`.
- `deploy/agalma.service` (systemd, `DynamicUser`, `StateDirectory=agalma`,
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`), `deploy/agalma.toml` example, `deploy/Caddyfile`
  example for proxy mode.
- Backup is `sqlite3 .backup` of `agalma.db` plus `rsync` of `blobs/`; documented in `docs/ops.md`.

---

## 17. Roadmap

Each phase ends at a gate that is a demonstration. Effort assumes one engineer part-time.

### Phase 0: Foundation and first page (about 1 week)

Goal: a page published with `curl` is served at `demo.localhost` from a single `agalma serve`.

1. `go.mod` and an empty `cmd/agalma/agalma.go` already exist. Add the `serve` subcommand
   dispatch, `buildinfo`, `config` with Load and Validate and tests, `docs/adr/0001-language.md`
   and `0002-storage.md`.
2. `sqlite`: Open with pragmas (WAL, busy timeout, foreign keys), migrator, `0001_init.sql` with the
   artifact, version, version_file, blob, user, api_key, artifact_actor, audit_event tables.
3. `blob`: interface, `FS`, `blobtest`.
4. `artifact`: types, validation (slug, path, metadata) with tables of cases, errors, `Store`
   interface, `storetest`, `Service.Create`/`Update`/`Get`/`List`/`ResolvePublished`/`Files`.
5. `sqlite` implements `artifact.Store`; passes `storetest`.
6. `site`: `ParseHost`, handler, headers, 404 pages; tests with a fake resolver.
7. `certs`: `SelfSigned`, `Files`, `TLSConfig`.
8. `middleware`; `httpapi` user routes for create (JSON and multipart), get, list, update, files;
   bearer auth with a single bootstrap API key read from config for now.
9. `app`: wiring, `ServeMux` host routing, HTTPS and HTTP listeners, proxy mode, graceful shutdown;
   first end-to-end test.

Gate: `curl -T` a tar.gz to `api.localhost`, open `https://demo.localhost:8443/` with the
self-signed wildcard, see the page; the e2e test does the same in CI.

### Phase 1: Core host (2 to 3 weeks)

1. `user`: users, keys, verification; `sqlite` implements `user.Store`; `auth` package replaces the
   bootstrap key; admin token middleware; admin API users and keys routes.
2. Remaining user API: PATCH, publish (rollback), delete, stale/unstale, `/me`; `If-Match`;
   `expected_version` conflict path; per-user quota.
3. `certs.ACME` with CertMagic and libdns providers (cloudflare, route53, hetzner); staging-CA
   dry run documented; renewal loop.
4. `cli`: `publish <dir>`, `update`, `get`, `list`, `rm`, `stale`, `unstale`, `visibility`,
   `files cat`, `me`, with `--json`; config from `AGALMA_URL` and `AGALMA_TOKEN` or
   `~/.config/agalma/config.toml`.
5. `deploy/`: Dockerfile, systemd unit, example config and Caddyfile; `docs/install.md`.

Gate: an artifact published from the CLI on a VPS is live at `<slug>.<base>` with a Let's Encrypt
wildcard certificate; a second publish creates version 2 and `<slug>--v1.<base>` still serves the
first.

### Phase 2a: MCP server with API keys (1 to 2 weeks)

1. `api` wire types shared with `httpapi`; `mcpserver` with the ten core tools, two resource
   templates, `publish-page` prompt; stateless Streamable HTTP handler; principal from the bearer
   middleware.
2. `upload` package, its `sqlite` store and the `uploads-expire` job; REST `POST /uploads`,
   `PATCH /uploads/{id}/files/{path}`, `DELETE /uploads/{id}`; `upload_id` accepted by the REST
   create and update handlers; `begin_upload` and `upload_chunk` tools; `upload_id` on the create
   and update tools.
3. In-memory transport tests for every tool, including a chunked publish of a multi-megabyte
   bundle; HTTP test for the `401` shape; Origin and protocol version checks.
4. `SKILL.md` for Claude Code and `docs/mcp.md` with `claude mcp add` instructions and the chunked
   upload recipe.

Gate: from Claude Code, with `claude mcp add --transport http --header "Authorization: Bearer ..."`,
create a page, read it back, update it, list it, mark it stale, and publish a bundle larger than the
inline guidance through `begin_upload` and `upload_chunk`; verify with the MCP Inspector.

### Phase 2b: OAuth for claude.ai (1 to 2 weeks)

1. `oauth` package: metadata endpoints, DCR, CIMD fetch and cache, authorize with pending
   `oauth_request` rows, token with PKCE and refresh rotation, `oauth.Store` in `sqlite`;
   `auth.Chain` wires it in. API-key fallback consent page first, so the flow is testable end to
   end before any identity provider exists.
2. `oidc` package over go-oidc; `user` gains email, identities and `ResolveIdentity` with the
   provisioning policy; migration adds `user.email`, `user_identity`, `oauth_request`; the
   callback and the one-click consent page.
3. Protected-resource metadata on the MCP host; `401` with `resource_metadata`.
4. Admin API: create users with email, link and unlink identities, list and revoke OAuth grants.
5. `docs/identity.md`: registering the redirect URI in Entra ID and Google Workspace, the claims
   each sends, and the provisioning policy.

Gate: add `https://mcp.<base>/` as a claude.ai custom connector, sign in through Microsoft Entra
ID, approve the consent page, publish a page from claude.ai, then update it from Claude Code with no
manual file copying. Verify Claude Code's own OAuth flow (CIMD plus loopback)
works as well as the bearer path, and that the API-key fallback works on an instance with no
provider configured.

### Phase 3: Lifecycle, admin, release (2 to 3 weeks)

1. `scheduler`; `Sweep` and `CollectGarbage`; tombstones; audit events on every transition; admin
   jobs routes.
2. Rate limits, Prometheus metrics, structured request logs, `/health`.
3. Admin: artifacts listing with sizes, transfer, hide, settings, audit query.
4. Security review of `site` and `artifact` validation; `docs/security.md`; `govulncheck` in CI.
5. goreleaser, SBOM, cosign, Docker image; `docs/` complete (install, ops, API, MCP, CLI).

Gate: v1.0.0 tagged with signed binaries for linux/amd64, linux/arm64, darwin/arm64; the e2e test
drives an artifact from published to stale to soft-deleted to purged to tombstone with a fast clock.

### Phase 4: Beyond v1 (unscheduled)

Gated visibility with access keys exchanged for a host-scoped cookie; S3 blob store (same `blob.Store`
interface, `blobtest` suite); Postgres store (`storetest` suite); idle expiry from a per-slug
last-hit day; stale banner; gallery and landing page at the apex (signed in through the same OIDC
provider); runtime-mutable settings; custom apex domains via on-demand TLS; Markdown-to-page
rendering; Enterprise Managed Auth (RFC 7523 jwt-bearer grant with per-tenant trusted issuers);
several identity providers per instance; public-instance hardening checklist.

---

## 18. Decision log and verification items

Decided on 2026-10-24 and recorded in section 1:

| Decision                          | Choice                                                                 |
| --------------------------------- | ---------------------------------------------------------------------- |
| Licence                           | MIT                                                                    |
| Default write policy              | `shared`                                                               |
| Module path                       | `agalma` (binaries and Docker image, not `go install`)                 |
| Sign-in for the OAuth consent flow| Organisation's OIDC provider; API-key fallback when none is configured |
| First provider tested live        | Microsoft Entra ID                                                     |
| Provisioning default              | `auto_provision = true` within `allowed_domains`                       |
| Enterprise Managed Auth           | Phase 4, unscheduled                                                   |
| Large uploads over MCP            | Chunked staging: `begin_upload`, `upload_chunk`, `upload_id` on create and update |

Nothing is waiting on a decision. Items to verify during the build rather than decide now:

- **Inline guidance value.** `inline_guidance_bytes` defaults to 2 MiB; measure what claude.ai and
  Claude Code actually send comfortably in one tool call in Phase 2a and adjust the default.
- **Claude Code OAuth.** Claude Code supports both a bearer header and its own OAuth client (CIMD
  with loopback redirects). 2a gates on the header; 2b verifies the OAuth path too.
- **Entra claims.** Confirm whether the tenant's app registration emits `email` or only
  `preferred_username`, and document the optional-claim setting in `docs/identity.md`.
- **Google Workspace** gets a live run after Entra, before v1.0, since the fake provider cannot
  exercise the `hd` claim handling against real tokens.
