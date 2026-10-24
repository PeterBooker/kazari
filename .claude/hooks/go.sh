#!/bin/sh
# Claude Code hooks for Go, wired in .claude/settings.json.
#   format: after Claude edits a .go file, gofmt it; note that Go (or go.mod/go.sum) changed.
#   vet:    when Claude stops and Go changed since the last vet, run go vet ./...
#           Exit 2 hands gofmt and vet errors back to Claude.
input=$(cat)
marker="${TMPDIR:-/tmp}/kazari-go-changed-$(printf '%s' "$input" | jq -r .session_id)"

case $1 in
format)
	# Filtered here, not with "if": an Edit(**/*.go) condition skips Write calls.
	file=$(printf '%s' "$input" | jq -r .tool_input.file_path)
	case $file in
	*.go) touch "$marker" && gofmt -w "$file" || exit 2 ;;
	*/go.mod | */go.sum) touch "$marker" ;;
	esac
	;;
vet)
	[ -e "$marker" ] || exit 0
	rm -f "$marker"
	cd "$CLAUDE_PROJECT_DIR" && go vet ./... >&2 || exit 2
	;;
esac
