# Hugo

## Check

```sh
grep -n 'HUGO_VERSION:' .github/workflows/pages.yml
git ls-remote --tags --refs https://github.com/gohugoio/hugo | cut -d/ -f3 \
  | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1
hugo version
```

## Update

- Set `HUGO_VERSION` in `.github/workflows/pages.yml` and the version in the `go install github.com/gohugoio/hugo@v...` line in `README.md`. Check that the release still ships `hugo_<version>_linux-amd64.tar.gz` and `hugo_<version>_checksums.txt`, the names the workflow downloads.
- Install the same version locally: `go install github.com/gohugoio/hugo@v<version>`.

## Verify

`hugo --source site --gc --minify` must finish with no `WARN` and no `ERROR` lines. Hugo deprecates template functions and config keys often (`.Site.Data` became `hugo.Data`, `languageCode` became `locale`): fix each deprecation the way its message says, then rebuild. Then look at `hugo server --source site` (<http://localhost:1313/kazari/>): the home page, one docs page and a search.
