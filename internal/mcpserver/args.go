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
// cannot read is returned as it is, for the SDK to refuse. Values stay raw
// JSON, so the content of large files is neither decoded nor re-encoded.
func normalizeArgs(tool string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return raw
	}
	changed := numbers(args)
	switch tool {
	case "edit_file":
		if _, has := args["edits"]; !has {
			edit := map[string]json.RawMessage{}
			for _, f := range editFields {
				if v, ok := args[f]; ok {
					edit[f] = v
					delete(args, f)
				}
			}
			if len(edit) > 0 {
				list, err := json.Marshal([]map[string]json.RawMessage{edit})
				if err != nil {
					return raw
				}
				args["edits"], changed = list, true
			}
		}
		changed = editList(args) || changed
	case "update_artifact", "create_artifact":
		changed = fileList(args) || changed
		changed = fileEdits(args) || changed
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

// kind is the first byte of a JSON value: '{', '[', '"', a digit, '-', 't', 'f' or 'n'.
func kind(v json.RawMessage) byte {
	v = bytes.TrimLeft(v, " \t\r\n")
	if len(v) == 0 {
		return 0
	}
	return v[0]
}

func numbers(args map[string]json.RawMessage) bool {
	changed := false
	for _, k := range numberArgs {
		v, ok := args[k]
		if !ok || kind(v) != '"' {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		if _, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			args[k], changed = json.RawMessage(strings.TrimSpace(s)), true
		}
	}
	return changed
}

// editList makes m["edits"] a list and renames aliased fields in each edit.
func editList(m map[string]json.RawMessage) bool {
	changed := false
	if kind(m["edits"]) == '{' {
		m["edits"], changed = append(append(json.RawMessage{'['}, m["edits"]...), ']'), true
	}
	if kind(m["edits"]) != '[' {
		return changed
	}
	var list []json.RawMessage
	if json.Unmarshal(m["edits"], &list) != nil {
		return changed
	}
	renamed := false
	for i, e := range list {
		if kind(e) != '{' {
			continue
		}
		var edit map[string]json.RawMessage
		if json.Unmarshal(e, &edit) != nil {
			continue
		}
		aliased := false
		for alias, name := range editAliases {
			if v, ok := edit[alias]; ok {
				if _, taken := edit[name]; !taken {
					edit[name] = v
				}
				delete(edit, alias)
				aliased = true
			}
		}
		if !aliased {
			continue
		}
		out, err := json.Marshal(edit)
		if err != nil {
			continue
		}
		list[i], renamed = out, true
	}
	if !renamed {
		return changed
	}
	out, err := json.Marshal(list)
	if err != nil {
		return changed
	}
	m["edits"] = out
	return true
}

// fileList turns files sent as {path: content} or {path: {...}} into the list
// the tools take.
func fileList(args map[string]json.RawMessage) bool {
	if kind(args["files"]) != '{' {
		return false
	}
	var files map[string]json.RawMessage
	if json.Unmarshal(args["files"], &files) != nil {
		return false
	}
	list := make([]json.RawMessage, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		v := files[path]
		var entry map[string]json.RawMessage
		switch kind(v) {
		case '"':
			entry = map[string]json.RawMessage{"content": v}
		case '{':
			if json.Unmarshal(v, &entry) != nil {
				return false
			}
		default:
			return false
		}
		if _, has := entry["path"]; !has {
			p, err := json.Marshal(path)
			if err != nil {
				return false
			}
			entry["path"] = p
		}
		out, err := json.Marshal(entry)
		if err != nil {
			return false
		}
		list = append(list, out)
	}
	out, err := json.Marshal(list)
	if err != nil {
		return false
	}
	args["files"] = out
	return true
}

// fileEdits applies editList to each file. Only files whose JSON names
// "edits" are decoded, so a call that sends content alone is not copied.
func fileEdits(args map[string]json.RawMessage) bool {
	if kind(args["files"]) != '[' || !bytes.Contains(args["files"], []byte(`"edits"`)) {
		return false
	}
	var files []json.RawMessage
	if json.Unmarshal(args["files"], &files) != nil {
		return false
	}
	changed := false
	for i, f := range files {
		if kind(f) != '{' || !bytes.Contains(f, []byte(`"edits"`)) {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(f, &m) != nil || !editList(m) {
			continue
		}
		out, err := json.Marshal(m)
		if err != nil {
			continue
		}
		files[i], changed = out, true
	}
	if !changed {
		return false
	}
	out, err := json.Marshal(files)
	if err != nil {
		return false
	}
	args["files"] = out
	return true
}
