#!/bin/sh
# Hooks wired in .claude/settings.json. format: gofmt an edited .go file and note that Go changed.
# vet: on stop, go vet ./... if Go changed. Exit 2 hands errors back to Claude.
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
