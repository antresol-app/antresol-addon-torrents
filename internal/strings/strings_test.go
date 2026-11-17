package strings

import (
	"bufio"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	gostrings "strings"
	"testing"
	"unicode"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
)

// moduleRoot is the addon module (this package is internal/strings).
const moduleRoot = "../.."

// ctxSep is the corecopy i18n's context separator (gettext's msgctxt).
const ctxSep = "\x04"

// skipDirs are not the addon's own code: the copied core helpers (the
// core's own test covers them), this package (the catalogs), the web part,
// test data and dot folders.
func skipDir(rel, name string) bool {
	switch rel {
	case "internal/corecopy", "internal/strings", "web", "legacy":
		return true
	}
	return name == "testdata" || name == "node_modules" || (gostrings.HasPrefix(name, ".") && name != ".")
}

// goSources parses every non-test Go file of the module that the build
// compiles (fenced //go:build ignore files are left out), by path relative
// to the module root.
func goSources(t *testing.T) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	bctx := build.Default
	err := filepath.WalkDir(moduleRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(moduleRoot, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !gostrings.HasSuffix(p, ".go") || gostrings.HasSuffix(p, "_test.go") {
			return nil
		}
		if ok, err := bctx.MatchFile(filepath.Dir(p), d.Name()); err != nil || !ok {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files[rel] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 50 {
		t.Fatalf("only %d files under %s", len(files), moduleRoot)
	}
	return files, fset
}

func strLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// pluralForms are the Russian forms of the plurals this package registers
// (RegisterPlural(EN, one, few, many, …) in its catalogs): the corecopy
// i18n keeps them unexported, so they are read from the source.
func pluralForms(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	names, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, n := range names {
		if gostrings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok || len(call.Args) < 4 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "RegisterPlural" {
				return true
			}
			var forms [3]string
			for j := range 3 {
				s, ok := strLit(call.Args[1+j])
				if !ok {
					return true
				}
				forms[j] = s
			}
			if !i18n.HasPlural(i18n.EN, forms[0], forms[1], forms[2]) {
				t.Errorf("%s: plural %v not registered", fset.Position(call.Pos()), forms)
			}
			for _, s := range forms {
				out[s] = true
			}
			return true
		})
	}
	return out
}

// keepRules are the Russian literals deliberately left out of the
// catalogs: testdata/keep/*.txt (moved with the family from the core's
// internal/i18n/testdata/keep), one rule per line —
//
//	internal/pkg/file.go            # every literal of the file (reason)
//	internal/pkg/file.go: текст     # one literal (reason)
//
// "#" starts the reason; a reason is required.
func keepRules(t *testing.T) (files map[string]bool, lits map[string]map[string]bool) {
	t.Helper()
	files, lits = map[string]bool{}, map[string]map[string]bool{}
	paths, _ := filepath.Glob("testdata/keep/*.txt")
	if len(paths) == 0 {
		t.Fatal("no keep lists in testdata/keep")
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		ln := 0
		for sc.Scan() {
			ln++
			line := gostrings.TrimSpace(sc.Text())
			if line == "" || gostrings.HasPrefix(line, "#") {
				continue
			}
			rule, reason, ok := gostrings.Cut(line, " #")
			if !ok || gostrings.TrimSpace(reason) == "" {
				t.Errorf("%s:%d: a keep rule needs a reason after \" #\"", p, ln)
			}
			rule = gostrings.TrimSpace(rule)
			file, lit, isLit := gostrings.Cut(rule, ": ")
			if !isLit {
				files[file] = true
				continue
			}
			if lits[file] == nil {
				lits[file] = map[string]bool{}
			}
			lits[file][lit] = true
		}
		f.Close()
	}
	return files, lits
}

// TestRussianLiteralsTranslated: every Cyrillic string literal in the
// addon's code (comments, tests and the copied core helpers aside) is a
// key of the corecopy i18n's English catalog, the Russian form of a
// registered plural, or kept on purpose (testdata/keep).
func TestRussianLiteralsTranslated(t *testing.T) {
	files, fset := goSources(t)
	keepFiles, keepLits := keepRules(t)
	known := pluralForms(t)
	for _, l := range i18n.Languages[1:] {
		for _, k := range i18n.Keys(l) {
			known[k] = true
			if _, after, ok := gostrings.Cut(k, ctxSep); ok {
				known[after] = true
			}
		}
	}
	var missing []string
	for rel, f := range files {
		if keepFiles[rel] {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			s, ok := n.(*ast.BasicLit)
			if !ok || s.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(s.Value)
			if err != nil || !hasCyrillic(v) || known[v] || keepLits[rel][v] {
				return true
			}
			missing = append(missing, fset.Position(s.Pos()).String()+": "+strconv.Quote(v))
			return true
		})
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("untranslated Russian literal %s", m)
	}
}

// TestKeepRulesName: every keep rule names a file of the module (a stale
// rule after a move or a delete fails).
func TestKeepRulesName(t *testing.T) {
	keepFiles, keepLits := keepRules(t)
	for f := range keepFiles {
		if _, err := os.Stat(filepath.Join(moduleRoot, f)); err != nil {
			t.Errorf("keep rule for %s: %v", f, err)
		}
	}
	for f := range keepLits {
		if _, err := os.Stat(filepath.Join(moduleRoot, f)); err != nil {
			t.Errorf("keep rule for %s: %v", f, err)
		}
	}
}
