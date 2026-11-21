# GitHub Actions and goreleaser

The workflows name each action by the full tag of a release (`@v7.0.1`), and `.github/workflows/actions.lock` records the commit each tag resolves to, also for the actions an action uses itself, so a moved or compromised tag cannot change what CI and the release job run. `gh actions-lock` writes the lockfile; never edit it by hand. It is a `gh` extension, installed once with `gh extension install github/gh-actions-lock`. The example workflow in `site/content/docs/publish-from-code.md` stays on the major tag (`@v7`), which readers copy.

goreleaser: `version: "~> v2"` in `release.yml` keeps it on v2, and `.goreleaser.yaml` has `version: 2`. The major of `goreleaser/goreleaser-action` is separate from goreleaser's.

## Check

Every action in use with the release it is pinned to, then the newest release tag of each repository. `git ls-remote` needs no token and has no rate limit, unlike the GitHub API (60 unauthenticated requests an hour, which this loop can use up):

```sh
grep -rhoE 'uses: [^ ]+@[^ ]+' .github/workflows site/content | sort -u
grep -n 'version: "~>' .github/workflows/release.yml
for repo in $(grep -rhoE 'uses: [^@ ]+' .github/workflows site/content | cut -d' ' -f2 | cut -d/ -f1-2 | sort -u) \
    goreleaser/goreleaser; do
  echo "$repo $(git ls-remote --tags --refs "https://github.com/$repo" | cut -d/ -f3 \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1)"
done
goreleaser --version
```

`cut -d/ -f1-2` turns an action path such as `anchore/sbom-action/download-syft` into its repository.

## Update

- Move every use of an action (the check's first `grep` lists them) to the same release, as the full tag: `uses: actions/checkout@v7.0.1`. Then write the lockfile again, also after adding a workflow or removing a `uses:`:

  ```sh
  gh actions-lock
  ```

  Read its diff: only the entries of the actions you moved change, plus the actions those use themselves. A changed `owner_id` or `repo_id` means another repository now has the name: stop and ask.

  A new major also moves the `@vN` example in `publish-from-code.md`.
- `sigstore/cosign-installer`: a new major can install a new cosign major. Check that the `signs` and `docker_signs` arguments in `.goreleaser.yaml` and the `cosign verify-blob` command in `docs/install.md` still work with it.
- goreleaser v3 is a config migration: ask first.

## Verify

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest
gh actions-lock --verify
goreleaser check
```

Workflows cannot run locally: report that the changed ones still need a run on GitHub. For a changed release workflow, the snapshot build in `docker.md` is the closest local check.
