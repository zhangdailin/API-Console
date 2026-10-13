package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"orchids-api/internal/channel"
)

const modulePath = "orchids-api"

// portPackage declares the channel contract and must stay free of every
// implementation of it.
const portPackage = "internal/provider"

// linkPackage is the one list of channel imports for a binary or a test.
const linkPackage = "internal/provider/all"

// entrypoints drive channels through the port. A channel implementation must
// never reach back into one of them.
var entrypoints = []string{"internal/api", "internal/handler", "internal/refresh", "internal/modelrefresh"}

// channelImporters are the packages allowed to import a channel implementation
// directly, besides the channel itself: the link list, and the composition
// root, which mounts the one channel that serves inference through its own
// handler.
var channelImporters = []string{linkPackage, "cmd/server"}

// leafImports lists the packages everything else builds on, with the only
// module-internal imports each may have. A leaf that grows a dependency turns
// into a cycle risk for every package above it.
var leafImports = map[string][]string{
	"internal/util":     nil,
	"internal/errors":   nil,
	"internal/config":   nil,
	"internal/prompt":   nil,
	"internal/channel":  {"internal/util"},
	"internal/chatwire": {"internal/util"},
}

// channelPackages are the implementation packages, named after their channel.
func channelPackages() []string {
	definitions := channel.All()
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, "internal/"+string(definition.ID))
	}
	return out
}

func within(pkg, root string) bool {
	return pkg == root || strings.HasPrefix(pkg, root+"/")
}

func withinAny(pkg string, roots []string) bool {
	for _, root := range roots {
		if within(pkg, root) {
			return true
		}
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// internalImports maps every package under cmd/ and internal/ to the
// module-internal packages its non-test sources import.
func internalImports(t *testing.T) map[string][]string {
	t.Helper()
	root := moduleRoot(t)
	found := map[string]map[string]struct{}{}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			if found[pkg] == nil {
				found[pkg] = map[string]struct{}{}
			}
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if internal, ok := strings.CutPrefix(imported, modulePath+"/"); ok {
					found[pkg][internal] = struct{}{}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read imports under %s: %v", top, err)
		}
	}
	out := make(map[string][]string, len(found))
	for pkg, imports := range found {
		list := make([]string, 0, len(imports))
		for imported := range imports {
			list = append(list, imported)
		}
		sort.Strings(list)
		out[pkg] = list
	}
	return out
}

func sortedPackages(imports map[string][]string) []string {
	out := make([]string, 0, len(imports))
	for pkg := range imports {
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// TestChannelPortStaysFreeOfImplementations keeps the seam usable by every
// channel. The registry once lived next to imports of three channels, which is
// what made the fourth impossible to add without repeating the dispatch.
func TestChannelPortStaysFreeOfImplementations(t *testing.T) {
	imports := internalImports(t)
	channels := channelPackages()
	for _, imported := range imports[portPackage] {
		if withinAny(imported, channels) || within(imported, linkPackage) {
			t.Errorf("%s imports %s: the port must not know any channel implementation", portPackage, imported)
		}
	}
	if len(imports[portPackage]) == 0 {
		t.Fatalf("%s has no imports; the layering test is not reading the sources", portPackage)
	}
}

// TestChannelsAreReachedOnlyThroughThePort is the rule that makes a channel a
// unit: a consumer that imports one directly has to be edited for every new
// channel, and can drift from the others when it is.
func TestChannelsAreReachedOnlyThroughThePort(t *testing.T) {
	imports := internalImports(t)
	channels := channelPackages()
	for _, pkg := range sortedPackages(imports) {
		for _, imported := range imports[pkg] {
			for _, owner := range channels {
				if !within(imported, owner) || within(pkg, owner) {
					continue
				}
				if withinAny(pkg, channels) {
					t.Errorf("%s imports %s: one channel must not depend on another", pkg, imported)
					continue
				}
				if !withinAny(pkg, channelImporters) {
					t.Errorf("%s imports %s: reach a channel through %s instead", pkg, imported, portPackage)
				}
			}
		}
	}
}

// TestChannelsDoNotImportTheirCallers keeps the dependency pointing one way.
// An implementation that imports an entrypoint cannot be linked into that
// entrypoint's own tests.
func TestChannelsDoNotImportTheirCallers(t *testing.T) {
	imports := internalImports(t)
	channels := channelPackages()
	for _, pkg := range sortedPackages(imports) {
		if !withinAny(pkg, channels) {
			continue
		}
		for _, imported := range imports[pkg] {
			if withinAny(imported, entrypoints) || within(imported, "cmd") || within(imported, linkPackage) {
				t.Errorf("%s imports %s: a channel must not depend on the code that drives it", pkg, imported)
			}
		}
	}
}

func TestLeafPackagesStayLeaves(t *testing.T) {
	imports := internalImports(t)
	for leaf, allowed := range leafImports {
		if _, exists := imports[leaf]; !exists {
			t.Errorf("%s is listed as a leaf but has no sources", leaf)
			continue
		}
		for _, imported := range imports[leaf] {
			if !withinAny(imported, allowed) {
				t.Errorf("%s imports %s: a leaf package may import only %v from this module", leaf, imported, allowed)
			}
		}
	}
}
