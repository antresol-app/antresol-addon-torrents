package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestHookCoverage: every hook of antresol-addon.json
//   - is one the core knows: in KnownHooks of the core's
//     internal/addons/manifest.go (read by path: the addon cannot import
//     the core), or "tracking.scan:<kind>" with a kind its scanKindRe takes;
//   - is registered (a HookMux Handle("<hook>", fn) in the addon's code),
//     and nothing is registered the manifest does not declare;
//   - has a handler test: a test file of the package that registers it
//     names the hook; or the handler's name is declared in exactly one
//     package of the module and a test of that package or of the
//     registering one calls it (svc.Migrate.LeftoversHook, registered in
//     main, tested in internal/migrate). A call of a name many packages
//     declare (x.Close()) is no evidence. The manifest's own tests here
//     never count.
func TestHookCoverage(t *testing.T) {
	declared := manifestHooks(t, "antresol-addon.json")
	known, scanKind := coreKnownHooks(t)
	regs, tests := scanHookCode(t, ".")
	if problems := hookProblems(declared, known, scanKind, regs, tests); len(problems) > 0 {
		for _, p := range problems {
			t.Error(p)
		}
	}
}

// hookProblems is the guard's verdict, one line per gap.
func hookProblems(declared []string, known []string, scanKind *regexp.Regexp, regs map[string]hookReg, tests hookTests) []string {
	var out []string
	for _, h := range declared {
		if kind, ok := strings.CutPrefix(h, "tracking.scan:"); ok {
			if !scanKind.MatchString(kind) {
				out = append(out, h+": the core takes no such tracking.scan kind")
			}
		} else if !slices.Contains(known, h) {
			out = append(out, h+": not in the core's KnownHooks")
		}
		reg, ok := regs[h]
		if !ok {
			out = append(out, h+": declared in the manifest, but no Handle(\""+h+"\", …) registers it (add its handler, or drop the manifest line)")
			continue
		}
		if !tests.tested(h, reg) {
			out = append(out, h+": registered ("+reg.handler+" in "+reg.dir+"), but no test of that package names the hook or calls its handler")
		}
	}
	for h := range regs {
		if !slices.Contains(declared, h) {
			out = append(out, h+": registered, but the manifest does not declare it")
		}
	}
	slices.Sort(out)
	return out
}

func manifestHooks(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Hooks
}

// coreKnownHooks reads KnownHooks and scanKindRe from the core checkout:
// ANTRESOL_CORE (as make sets it), else the nearest folder above this one
// that holds internal/addons/manifest.go (the core repository this tree
// sits in). Without a core checkout (the addon's own repository) the test
// is skipped, as `make test` skips `corecopy --check`.
func coreKnownHooks(t *testing.T) ([]string, *regexp.Regexp) {
	t.Helper()
	core := os.Getenv("ANTRESOL_CORE")
	if core == "" {
		dir, err := filepath.Abs(".")
		if err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "internal", "addons", "manifest.go")); err == nil {
				core = dir
				break
			}
			up := filepath.Dir(dir)
			if up == dir {
				t.Skip("no core checkout (set ANTRESOL_CORE): the hooks are checked in the core repository")
			}
			dir = up
		}
	}
	path := filepath.Join(core, "internal", "addons", "manifest.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("the core's KnownHooks: %v", err)
	}
	var known []string
	var scanKind *regexp.Regexp
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		switch vs.Names[0].Name {
		case "KnownHooks":
			if cl, ok := vs.Values[0].(*ast.CompositeLit); ok {
				for _, e := range cl.Elts {
					if s, ok := stringLit(e); ok {
						known = append(known, s)
					}
				}
			}
		case "scanKindRe":
			if c, ok := vs.Values[0].(*ast.CallExpr); ok && len(c.Args) == 1 {
				if s, ok := stringLit(c.Args[0]); ok {
					scanKind = regexp.MustCompile(s)
				}
			}
		}
		return true
	})
	if len(known) == 0 || scanKind == nil {
		t.Fatalf("%s: no KnownHooks or scanKindRe found (%d hooks)", path, len(known))
	}
	return known, scanKind
}

// hookReg is a registration: the handler's last name and the package
// (directory) whose code registers it.
type hookReg struct{ handler, dir string }

type hookTests struct {
	names map[string]map[string]bool // dir → string literals in its test files
	calls map[string]map[string]bool // dir → selector names called in its test files
	decls map[string]map[string]bool // func or method name → the dirs whose non-test code declares it
}

// tested: a test of the registering package names h; or the handler's
// name is declared by one package only (so a call of it is a call of
// this handler, not of some other x.Close()) and a test of the
// registering or the declaring package calls it, or the declaring
// package's tests name h.
func (ts hookTests) tested(h string, r hookReg) bool {
	if ts.names[r.dir][h] {
		return true
	}
	if r.handler == "" || len(ts.decls[r.handler]) != 1 {
		return false
	}
	for d := range ts.decls[r.handler] {
		if ts.names[d][h] || ts.calls[d][r.handler] || ts.calls[r.dir][r.handler] {
			return true
		}
	}
	return false
}

func mark(m map[string]map[string]bool, k, v string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][v] = true
}

// manifestTests are this package's tests of the manifest itself: naming a
// hook there tests no handler.
var manifestTests = map[string]bool{"addon_test.go": true, "hooks_coverage_test.go": true}

// scanHookCode reads the module's Go files under dir: the hooks a
// non-test file registers (with its handler's last name, e.g.
// "LeftoversHook", and its package), what each package's test files name
// and call, and which packages declare which func and method names.
func scanHookCode(t *testing.T, dir string) (map[string]hookReg, hookTests) {
	t.Helper()
	regs := map[string]hookReg{}
	tests := hookTests{names: map[string]map[string]bool{}, calls: map[string]map[string]bool{}, decls: map[string]map[string]bool{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != dir && (strings.HasPrefix(n, ".") || n == "web" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(p, "_test.go")
		pkg := filepath.Dir(p)
		if isTest && pkg == filepath.Clean(dir) && manifestTests[filepath.Base(p)] {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if !isTest {
					mark(tests.decls, x.Name.Name, pkg)
				}
			case *ast.BasicLit:
				if isTest {
					if s, ok := stringLit(x); ok {
						mark(tests.names, pkg, s)
					}
				}
			case *ast.CallExpr:
				var name string
				switch fn := x.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				default:
					return true
				}
				if isTest {
					mark(tests.calls, pkg, name)
				} else if name == "Handle" && len(x.Args) == 2 {
					if h, ok := stringLit(x.Args[0]); ok && !strings.Contains(h, " ") && !strings.HasPrefix(h, "/") {
						regs[h] = hookReg{handler: lastName(x.Args[1]), dir: pkg}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return regs, tests
}

func stringLit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

// lastName is a handler expression's last identifier (d.pending →
// "pending", svc.Migrate.LeftoversHook → "LeftoversHook"); "" for others.
func lastName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// The guard's verdict on planted gaps: a declared hook nothing registers,
// one whose only "test" is an x.Close() call (even in its own package:
// many packages declare Close), one tested only
// in another package, one the core does not know, a registered one the
// manifest lacks; a handler declared in one package counts its tests.
func TestHookProblems(t *testing.T) {
	known := []string{"a.one", "a.two", "a.three", "a.close", "a.far", "a.moved"}
	scan := regexp.MustCompile(`^[a-z]+$`)
	regs := map[string]hookReg{
		"a.one":           {"One", "p"},
		"a.two":           {"two", "p"},
		"a.close":         {"Close", "p"},
		"a.far":           {"far", "p"},
		"a.moved":         {"LeftoversHook", "."},
		"tracking.scan:x": {"scan", "p"},
		"a.extra":         {"Extra", "p"},
	}
	tests := hookTests{
		names: map[string]map[string]bool{"p": {"a.two": true, "tracking.scan:x": true}, "q": {"a.far": true}},
		calls: map[string]map[string]bool{"p": {"One": true, "Close": true}, "q": {"Close": true}, "m": {"LeftoversHook": true}},
		decls: map[string]map[string]bool{"One": {"p": true}, "Close": {"p": true, "q": true}, "LeftoversHook": {"m": true}},
	}
	got := hookProblems([]string{"a.one", "a.two", "a.three", "a.close", "a.far", "a.moved", "tracking.scan:x", "tracking.scan:BAD", "b.unknown"},
		known, scan, regs, tests)
	want := []string{
		"a.close: registered (Close in p), but no test of that package names the hook or calls its handler",
		"a.extra: registered, but the manifest does not declare it",
		"a.far: registered (far in p), but no test of that package names the hook or calls its handler",
		"a.three: declared in the manifest, but no Handle(\"a.three\", …) registers it (add its handler, or drop the manifest line)",
		"b.unknown: declared in the manifest, but no Handle(\"b.unknown\", …) registers it (add its handler, or drop the manifest line)",
		"b.unknown: not in the core's KnownHooks",
		"tracking.scan:BAD: declared in the manifest, but no Handle(\"tracking.scan:BAD\", …) registers it (add its handler, or drop the manifest line)",
		"tracking.scan:BAD: the core takes no such tracking.scan kind",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
