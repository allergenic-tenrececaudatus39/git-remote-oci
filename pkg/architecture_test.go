package pkg_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/mrueg/git-remote-oci/pkg/"

// allowedImports is the dependency direction AGENTS.md describes.
//
// Keeping it here as well as in prose is the point: the rule exists to stop a
// cycle forming between the protocol layer and the registry client, and prose
// cannot fail a build. It was already out of date once - pkg/cli and pkg/gc
// were added and the documented graph never mentioned them.
var allowedImports = map[string][]string{
	"lfs":    {},
	"config": {},
	"oci":    {"lfs", "config"},
	"git":    {"lfs", "config"},
	"gc":     {"git", "oci", "lfs", "config"},
	// helper imports gc so a push can compact once enough commits have piled
	// up. One-way: gc knows nothing about the protocol, so no cycle and no
	// second package with an opinion about stdout.
	"helper": {"gc", "git", "oci", "lfs", "config"},
	"cli":    {"helper", "gc", "git", "oci", "lfs", "config"},
}

// TestInternalDependencyDirection pins the import graph.
//
// Two rules carry the weight. Nothing may import pkg/helper except pkg/cli,
// because helper owns stdout and the protocol state machine and must stay a
// leaf of the command layer. And pkg/oci must not import pkg/git: the registry
// client has no business knowing about the object store, and letting it would
// make the two impossible to reason about separately.
func TestInternalDependencyDirection(t *testing.T) {
	for pkg, allowed := range allowedImports {
		dir := filepath.Join("..", "pkg", pkg)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("pkg/%s is in the allow-list but does not exist; remove it here and from AGENTS.md", pkg)
			continue
		}
		for _, imported := range internalImports(t, dir) {
			if !slices.Contains(allowed, imported) {
				t.Errorf("pkg/%s imports pkg/%s, which the documented dependency direction does not allow.\n"+
					"If this is deliberate, update both this list and the Layout section of AGENTS.md.",
					pkg, imported)
			}
		}
	}
}

// TestEveryPackageIsAccountedFor catches a new package that nobody wrote down.
func TestEveryPackageIsAccountedFor(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "pkg"))
	if err != nil {
		t.Fatalf("read pkg/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, known := allowedImports[e.Name()]; !known {
			t.Errorf("pkg/%s is not in the dependency allow-list; add it here and to the Layout table in AGENTS.md", e.Name())
		}
	}
}

// internalImports returns the project-internal packages a directory imports,
// ignoring test files: a test may reach for anything it needs.
func internalImports(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var found []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil || !strings.HasPrefix(path, modulePath) {
				continue
			}
			pkg := strings.TrimPrefix(path, modulePath)
			if !slices.Contains(found, pkg) {
				found = append(found, pkg)
			}
		}
	}
	return found
}
