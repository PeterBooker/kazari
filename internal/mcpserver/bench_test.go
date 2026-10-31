package mcpserver_test

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
