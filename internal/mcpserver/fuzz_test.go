package mcpserver_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/PeterBooker/kazari/internal/mcpserver"
)

// FuzzNormalizeArgs holds the rewrite of a tool's arguments to what it may
// do: never panic, leave what it does not change byte for byte, keep a JSON
// object an object, and settle after one pass.
func FuzzNormalizeArgs(f *testing.F) {
	for _, s := range []string{
		`{"slug":"a","edits":[{"old_string":"x","new_string":"y"}],"expected_version":"4"}`,
		`{"slug":"a","old_text":"x","new_text":"y"}`,
		`{"edits":{"old_text":"x"}}`,
		`{"files":{"index.html":"<p>","a.js":{"content":"x"}}}`,
		`{"files":[{"path":"a","edits":{"old_string":"x"}}]}`,
		`{"version":" 12 ","start_line":"x"}`,
		`[]`, `null`, `{`, ``, `{"files":{"a":1}}`,
	} {
		for _, tool := range []string{"edit_file", "update_artifact", "create_artifact", "read_file"} {
			f.Add(tool, s)
		}
	}
	f.Fuzz(func(t *testing.T, tool, raw string) {
		out := mcpserver.NormalizeArgs(tool, json.RawMessage(raw))
		if bytes.Equal(out, []byte(raw)) {
			return
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil || obj == nil {
			t.Fatalf("NormalizeArgs(%q, %q) = %q, want a JSON object: %v", tool, raw, out, err)
		}
		again := mcpserver.NormalizeArgs(tool, out)
		if !bytes.Equal(again, out) {
			t.Errorf("NormalizeArgs(%q, %q) = %q, then %q; want it settled after one pass", tool, raw, out, again)
		}
	})
}
