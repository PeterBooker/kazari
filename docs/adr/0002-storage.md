# ADR 0002: SQLite metadata and content-addressed blobs

Status: accepted (2026-10-24)

## Context

Kazari stores artifact metadata (artifacts, versions, users, keys, tokens, audit log) and the files of every immutable version. It is self-hosted, usually on one small machine, and must build without CGO (ADR 0001). Versions are immutable and often share most of their files with the previous version. The serving path looks up one file of one version per request.

The first design stored each version's manifest (path to hash) as a JSON blob and used UUIDs as identifiers.

## Decision

- **SQLite through `github.com/ncruces/go-sqlite3`** (`driver` + `embed`): pure Go (a Wasm build on wazero), tracks upstream SQLite closely, and plugs into `database/sql`. `modernc.org/sqlite` is a one-line swap. SQL is hand-written; no ORM. The database lives at `<data_dir>/kazari.db`.
- **Blobs are content-addressed files** under `<data_dir>/blobs`, named by SHA-256. Uploads stream to a temp file while hashing and are renamed into place, so identical files across versions and artifacts are stored once and a write is atomic.
- **The version manifest is rows.** A `version_file` table holds one row per path in a version instead of a JSON manifest column. File lookup on the serving path is one indexed query, blob garbage collection is an anti-join against `version_file`, and no JSON is parsed on the hot path. Artifact `metadata` stays a JSON text column: it is opaque and only needs `json_extract` filters.
- **Natural keys externally, integers internally.** The API addresses artifacts by slug, versions by number, users by handle and API keys by their public prefix. Tables use `INTEGER PRIMARY KEY`; no row id ever appears in the API.

## Consequences

- No UUID dependency, smaller indexes and simpler joins; renaming a natural key (a slug) is an update to one column, not a cascade.
- Stores are coarse-grained and own their transactions, so a Postgres implementation could replace SQLite behind the same `Store` interfaces if a multi-node deployment is ever needed.
- One writer at a time; acceptable for an artifact host whose writes come from people and agents.
- Backup is `sqlite3 .backup` of `kazari.db` plus `rsync` of `blobs/`. Because blobs are never rewritten, an incremental copy is safe while the server runs.
- Blob GC must run against committed state only: a blob is removed when no `version_file` row and no open upload references it.
