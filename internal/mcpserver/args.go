package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
)

// numberArgs are the integer arguments a model may send as strings.
var numberArgs = []string{"expected_version", "base_version", "version", "start_line", "end_line", "offset", "limit",
	"files_before", "archive_bytes"}

// editAliases are the names other tools give edit fields, mapped to ours.
var editAliases = map[string]string{"old_string": "old_text", "new_string": "new_text"}

// editFields are the fields of one edit, for edits sent at the top level.
var editFields = []string{"old_text", "new_text", "old_string", "new_string", "replace_all", "start", "end", "after", "before"}

// argsMiddleware forgives the slips a model makes most often in a tool's
// arguments before the SDK validates them, and turns a validation failure
// into invalid_argument with the shape the tool expects.
func (b *builder) argsMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if method != "tools/call" || !ok {
				return next(ctx, method, req)
			}
			params.Arguments = normalizeArgs(params.Name, params.Arguments)
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
				if _, ours := errors.AsType[*toolError](r.GetError()); !ours {
					b.schemaError(params.Name, r)
				}
			}
			return res, err
		}
	}
}

// schemaError replaces the SDK's validation text, which names anonymous
// schemas, with an error that says what the tool takes.
func (b *builder) schemaError(tool string, r *mcp.CallToolResult) {
	detail := ""
	if cause := r.GetError(); cause != nil {
		detail = cause.Error()
	}
	for _, p := range []string{`validating "arguments": `, "validating root: "} {
		detail = strings.TrimPrefix(detail, p)
	}
	msg := "the arguments do not match the input schema of " + tool
	if detail != "" && !strings.Contains(detail, "<anonymous schema>") && len(detail) <= 300 {
		msg += ": " + detail
	}
	te := &toolError{e: api.Error{Code: api.CodeInvalidArgument, Message: msg}, next: argShape(tool), cause: r.GetError()}
	r.Content = nil
	r.SetError(te)
}

func argShape(tool string) string {
	switch tool {
	case "edit_file":
		return "Send {slug, path, edits: [{old_text, new_text}], expected_version}: edits is a list, and each edit " +
			"needs one of old_text, start, after or before, plus new_text (omitted, it deletes). Numbers are plain numbers."
	case "update_artifact":
		return "Send {slug, files: [{path, content}], expected_version}: files is a list, and each entry has content, " +
			"url, edits (as in edit_file) or delete: true. Numbers are plain numbers."
	case "create_artifact":
		return "Send {slug, name, description, files: [{path, content}]}: files is a list of objects, each with path " +
			"and content (encoding \"base64\" for binary files)."
	}
	return "Check the argument names and types against the tool's input schema, then call again."
}

// normalizeArgs rewrites raw when it holds a known slip: an integer as a
// string, edit fields under other tools' names, edits sent at the top level
// or as one object, and files sent as a map of path to content. Anything it
// cannot read is returned as it is, for the SDK to refuse.
func normalizeArgs(tool string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil || args == nil {
		return raw
	}
	changed := numbers(args)
	switch tool {
	case "edit_file":
		if _, has := args["edits"]; !has {
			edit := map[string]any{}
			for _, f := range editFields {
				if v, ok := args[f]; ok {
					edit[f] = v
					delete(args, f)
				}
			}
			if len(edit) > 0 {
				args["edits"], changed = []any{edit}, true
			}
		}
		changed = editList(args) || changed
	case "update_artifact", "create_artifact":
		changed = fileList(args) || changed
		if files, ok := args["files"].([]any); ok {
			for _, f := range files {
				if m, ok := f.(map[string]any); ok {
					changed = editList(m) || changed
				}
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return out
}

func numbers(args map[string]any) bool {
	changed := false
	for _, k := range numberArgs {
		if s, ok := args[k].(string); ok {
			if _, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				args[k], changed = json.Number(strings.TrimSpace(s)), true
			}
		}
	}
	return changed
}

// editList makes m["edits"] a list and renames aliased fields in each edit.
func editList(m map[string]any) bool {
	changed := false
	if one, ok := m["edits"].(map[string]any); ok {
		m["edits"], changed = []any{one}, true
	}
	list, _ := m["edits"].([]any)
	for _, e := range list {
		edit, ok := e.(map[string]any)
		if !ok {
			continue
		}
		for alias, name := range editAliases {
			if v, ok := edit[alias]; ok {
				if _, taken := edit[name]; !taken {
					edit[name] = v
				}
				delete(edit, alias)
				changed = true
			}
		}
	}
	return changed
}

// fileList turns files sent as {path: content} or {path: {...}} into the list
// the tools take.
func fileList(args map[string]any) bool {
	files, ok := args["files"].(map[string]any)
	if !ok {
		return false
	}
	list := make([]any, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		switch v := files[path].(type) {
		case string:
			list = append(list, map[string]any{"path": path, "content": v})
		case map[string]any:
			if _, has := v["path"]; !has {
				v["path"] = path
			}
			list = append(list, v)
		default:
			return false
		}
	}
	args["files"] = list
	return true
}
