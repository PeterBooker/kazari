package app_test

import (
	"errors"
	"go/build"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type layer string

const (
	// base packages know no other package of Kazari, and every package may
	// know them.
	base layer = "base"
	// leaf packages know no other package of Kazari but the base ones.
	leaf layer = "leaf"
	// platform packages know nothing of artifacts, users or grants.
	platform layer = "platform"
	domain   layer = "domain"
	// convert turns domain types into wire types for the surfaces.
	convert layer = "convert"
	// surface packages are what a host serves. None knows another.
	surface layer = "surface"
	store   layer = "store"
	// wiring is the one package that knows every other.
	wiring  layer = "wiring"
	command layer = "command"
	program layer = "program"
	// support packages hold conformance suites and fakes, for tests alone.
	support layer = "support"
)

var mayImport = map[layer][]layer{
	base:     {},
	leaf:     {base},
	platform: {base, leaf, platform},
	domain:   {base, leaf, platform, domain},
	convert:  {base, leaf, platform, domain},
	surface:  {base, leaf, platform, domain, convert},
	store:    {base, leaf, platform, domain},
	wiring:   {base, leaf, platform, domain, convert, surface, store},
	command:  {base, leaf, platform, domain, wiring},
	program:  {base, leaf, command},
	support:  {base, leaf, platform, domain},
}

var layers = map[string]layer{
	"cmd/kazari": program,

	"internal/cli": command,
	"internal/app": wiring,

	"internal/sqlite": store,

	"internal/account":   surface,
	"internal/dashboard": surface,
	"internal/httpapi":   surface,
	"internal/mcpserver": surface,
	"internal/site":      surface,

	"internal/present": convert,

	"internal/artifact":  domain,
	"internal/auth":      domain,
	"internal/backup":    domain,
	"internal/fetch":     domain,
	"internal/gate":      domain,
	"internal/oauth":     domain,
	"internal/oidc":      domain,
	"internal/passkey":   domain,
	"internal/ratelimit": domain,
	"internal/session":   domain,
	"internal/snapshot":  domain,
	"internal/upload":    domain,
	"internal/user":      domain,
	"internal/views":     domain,

	"internal/blob":       platform,
	"internal/certs":      platform,
	"internal/middleware": platform,
	"internal/preflight":  platform,
	"internal/scheduler":  platform,
	"internal/telemetry":  platform,
	"internal/update":     platform,

	"internal/api":        leaf,
	"internal/audit":      leaf,
	"internal/buildinfo":  leaf,
	"internal/config":     leaf,
	"internal/csp":        leaf,
	"internal/dirlock":    leaf,
	"internal/disk":       leaf,
	"internal/instrument": leaf,
	"internal/theme":      leaf,

	"internal/errcode": base,

	"internal/artifact/storetest":        support,
	"internal/blob/blobtest":             support,
	"internal/instrument/instrumenttest": support,
	"internal/leaktest":                  support,
	"internal/passkey/passkeytest":       support,
}

// restricted narrows who may import a path, or anything below it, beyond
// what the layers say. Paths of Kazari are relative to the module.
var restricted = []struct {
	path      string
	only      []string
	onlyLayer layer
}{
	// Packages take Options from internal/app; these two show and check a
	// whole configuration.
	{path: "internal/config", only: []string{"cmd/kazari", "internal/app", "internal/cli", "internal/dashboard", "internal/preflight"}},
	// The wire types are for what answers a client and for the CLI; a
	// domain package names an error code with internal/errcode. The rate
	// limiter answers a client itself.
	{path: "internal/api", only: []string{"internal/account", "internal/app", "internal/cli", "internal/httpapi",
		"internal/mcpserver", "internal/present", "internal/ratelimit"}},
	{path: "database/sql", only: []string{"internal/sqlite"}},
	{path: "github.com/ncruces/go-sqlite3", only: []string{"internal/sqlite"}},
	{path: "github.com/alecthomas/kong", only: []string{"internal/cli"}},
	{path: "github.com/google/go-cmp", onlyLayer: support},
	{path: "testing", onlyLayer: support},
}

func TestPackageDependencies(t *testing.T) {
	root := filepath.Join("..", "..")
	module := modulePath(t, root)
	pkgs := modulePackages(t, root)

	for name := range layers {
		if _, ok := pkgs[name]; !ok {
			t.Errorf("%s has a layer in deps_test.go but no Go files", name)
		}
	}
	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		pkg := pkgs[name]
		from, ok := layers[name]
		if !ok {
			t.Errorf("%s has no layer in deps_test.go", name)
			continue
		}
		for _, imp := range pkg.Imports {
			rel, own := relative(module, imp)
			if own {
				to, ok := layers[rel]
				switch {
				case !ok:
					t.Errorf("%s imports %s, which has no layer in deps_test.go", name, rel)
				case !slices.Contains(mayImport[from], to):
					t.Errorf("%s imports %s: a %s package may not import a %s package", name, rel, from, to)
				}
			}
			for _, r := range restricted {
				if rel != r.path && !strings.HasPrefix(rel, r.path+"/") {
					continue
				}
				if slices.Contains(r.only, name) || (r.onlyLayer != "" && from == r.onlyLayer) {
					continue
				}
				who := strings.Join(r.only, ", ")
				if r.onlyLayer != "" {
					who = "the " + string(r.onlyLayer) + " packages"
				}
				t.Errorf("%s imports %s: only %s may import %s", name, rel, who, r.path)
			}
		}

		// Tests run against the real store, the conformance suites and at
		// times a surface, so the layers do not bind them. A leaf stays a
		// leaf: a test that needs another package belongs to that package.
		if from != leaf && from != base {
			continue
		}
		for _, imp := range slices.Concat(pkg.TestImports, pkg.XTestImports) {
			rel, own := relative(module, imp)
			if own && rel != name && layers[rel] != support {
				t.Errorf("the tests of %s import %s: a %s package's tests may import only it and a support package", name, rel, from)
			}
		}
	}
}

// relative returns imp as the rules name it: relative to the module for a
// package of Kazari, unchanged otherwise.
func relative(module, imp string) (rel string, own bool) {
	if rel, ok := strings.CutPrefix(imp, module+"/"); ok {
		return rel, true
	}
	return imp, false
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if mod, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(mod)
		}
	}
	t.Fatalf("no module line in %s", filepath.Join(root, "go.mod"))
	return ""
}

// modulePackages reads every package under root, keyed by its path in the
// module. It reads the files of every platform and build tag, so a rule
// holds for all of them.
func modulePackages(t *testing.T, root string) map[string]*build.Package {
	t.Helper()
	ctx := build.Default
	ctx.UseAllFiles = true
	pkgs := map[string]*build.Package{}
	err := filepath.WalkDir(root, func(dir string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); dir != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
			return filepath.SkipDir
		}
		pkg, err := ctx.ImportDir(dir, 0)
		if _, none := errors.AsType[*build.NoGoError](err); none {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		pkgs[path.Clean(filepath.ToSlash(rel))] = pkg
		return nil
	})
	if err != nil {
		t.Fatalf("reading the packages under %s: %v", root, err)
	}
	return pkgs
}
