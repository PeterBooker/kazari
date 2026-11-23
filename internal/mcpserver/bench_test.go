package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/mcpserver"
)

func BenchmarkToolCall(b *testing.B) {
	e := newEnv(b)
	cs := e.connect(b, member)
	ctx := context.Background()
	create, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "create_artifact", Arguments: map[string]any{
		"slug": "bench", "name": "Bench", "description": "A benchmark page",
		"files": []map[string]any{{"path": "index.html", "content": "<h1>Bench</h1>"}},
	}})
	if err != nil || create.IsError {
		b.Fatalf("create_artifact: %v %+v", err, create)
	}
	params := &mcp.CallToolParams{Name: "get_artifact", Arguments: map[string]any{"slug": "bench"}}
	b.ReportAllocs()
	for b.Loop() {
		res, err := cs.CallTool(ctx, params)
		if err != nil || res.IsError {
			b.Fatalf("get_artifact: %v %+v", err, res)
		}
	}
}

// largeFiles is a megabyte of text and a megabyte of binary in base64, the
// arguments the normalizer and the decoder see on a large inline publish.
func largeFiles() []map[string]any {
	bin := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3}, 1<<17)
	return []map[string]any{
		{"path": "index.html", "content": strings.Repeat("<p>Bench</p>\n", 1<<20/13)},
		{"path": "logo.png", "content": base64.StdEncoding.EncodeToString(bin), "encoding": "base64"},
	}
}

func BenchmarkLargeUpdate(b *testing.B) {
	e := newEnv(b)
	cs := e.connect(b, member)
	ctx := context.Background()
	files := largeFiles()
	create, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "create_artifact", Arguments: map[string]any{
		"slug": "bench", "name": "Bench", "description": "A benchmark page", "files": files,
	}})
	if err != nil || create.IsError {
		b.Fatalf("create_artifact: %v %+v", err, create)
	}
	params := &mcp.CallToolParams{Name: "update_artifact", Arguments: map[string]any{"slug": "bench", "files": files}}
	b.ReportAllocs()
	for b.Loop() {
		res, err := cs.CallTool(ctx, params)
		if err != nil || res.IsError {
			b.Fatalf("update_artifact: %v %+v", err, res)
		}
	}
}

func BenchmarkNormalizeArgs(b *testing.B) {
	for _, c := range []struct {
		name    string
		version any
	}{{"unchanged", 3}, {"rewritten", "3"}} {
		b.Run(c.name, func(b *testing.B) {
			raw, err := json.Marshal(map[string]any{"slug": "bench", "expected_version": c.version, "files": largeFiles()})
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				mcpserver.NormalizeArgs("update_artifact", raw)
			}
		})
	}
}
