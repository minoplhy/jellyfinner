// Package archtest checks the layout rules in the README's "Code layout".
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sqlImports may only appear in a database backend.
var sqlImports = []string{"database/sql", "github.com/mattn/go-sqlite3"}

// backendPrefix is where backends live. Only cmd/* picks one; library code
// goes through database.Store.
const backendPrefix = "jellyfinner/internal/database/"

func TestImportRules(t *testing.T) {
	root := filepath.Join("..", "..")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "deploy") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		inDatabase := strings.HasPrefix(rel, "internal/database/")
		inCmd := strings.HasPrefix(rel, "cmd/")
		isTest := strings.HasSuffix(rel, "_test.go")
		for _, spec := range file.Imports {
			imp, _ := strconv.Unquote(spec.Path.Value)
			for _, banned := range sqlImports {
				if imp == banned && !inDatabase {
					t.Errorf("%s imports %s: only a backend under internal/database/ may", rel, imp)
				}
			}
			isBackend := strings.HasPrefix(imp, backendPrefix) && imp != backendPrefix+"dbtest"
			if isBackend && !inDatabase && !inCmd && !isTest {
				t.Errorf("%s imports %s: only cmd/* chooses a backend; use database.Store", rel, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("checked only %d files: is the walk rooted at the module?", checked)
	}
}

// The worker and edge agent build as static binaries with CGO_ENABLED=0, so
// nothing they import, however indirectly, may pull in the database.
func TestNodeBinariesStayFreeOfTheDatabase(t *testing.T) {
	for _, cmd := range []string{"jellyfinner/cmd/worker", "jellyfinner/cmd/edge-agent"} {
		out, err := exec.Command("go", "list", "-deps", cmd).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", cmd, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if dep == "jellyfinner/internal/database" || strings.HasPrefix(dep, backendPrefix) || slices.Contains(sqlImports, dep) {
				t.Errorf("%s depends on %s", cmd, dep)
			}
		}
	}
}
