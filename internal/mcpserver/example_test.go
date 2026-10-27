package mcpserver_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/mcpserver"
	"github.com/PeterBooker/kazari/internal/present"
)

var updateExample = flag.Bool("update", false, "rewrite examples/kazari/SKILL.md from the default settings")

// examplePath is the committed skill for the default settings, relative to this package.
const examplePath = "../../examples/kazari/SKILL.md"

// exampleOptions mirrors how internal/app builds the MCP options from a config,
// for config.Default() with placeholder hosts.
func exampleOptions() mcpserver.Options {
	cfg := config.Default()
	return mcpserver.Options{
		InlineGuidanceBytes: int64(cfg.Limits.InlineGuidanceBytes),
		MaxVersionBytes:     int64(cfg.Limits.MaxVersionBytes),
		MaxFileBytes:        int64(cfg.Limits.MaxFileBytes),
		MaxFiles:            cfg.Limits.MaxFiles,
		MaxChunkBytes:       int64(cfg.Limits.MaxChunkBytes),
		MaxTotalBytes:       int64(cfg.Limits.MaxVersionBytes),
		CSP:                 cfg.Serve.CSP,
		ReservedSlugs:       cfg.Serve.ReservedSlugs,
		APIBaseURL:          "https://api.<base>/v1",
		MCPURL:              "https://mcp.<base>/",
		DefaultVisibility:   cfg.Serve.DefaultVisibility,
		MaxShareLinkTTL:     cfg.Serve.ShareLinkMaxTTL.Std(),
	}
}

// TestExampleSkill keeps examples/kazari/SKILL.md equal to the skill an instance
// with the default settings serves. Refresh it with
// go test ./internal/mcpserver -run TestExampleSkill -update.
func TestExampleSkill(t *testing.T) {
	want := mcpserver.Skill(present.Links{}, exampleOptions())
	path := filepath.FromSlash(examplePath)
	if *updateExample {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s differs from the generated skill; run go test ./internal/mcpserver -run TestExampleSkill -update", examplePath)
	}
}
