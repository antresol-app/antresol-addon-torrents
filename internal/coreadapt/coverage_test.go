package coreadapt_test

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestMain notes the replay pairs (replay_test.go) while the package's
// tests run, then runs TestAdapterCoverage last: only on a run of the
// whole package (no -run, -skip or -list), since a filtered run makes
// fewer pairs.
func TestMain(m *testing.M) {
	http.DefaultTransport = recording{next: http.DefaultTransport}
	code := m.Run()
	if code == 0 && !wholeRun() {
		fmt.Println("TestAdapterCoverage: skipped (filtered run: -run, -skip or -list)")
	}
	if code == 0 && wholeRun() {
		if missing := adapterCoverage(); len(missing) > 0 {
			fmt.Println("--- FAIL: TestAdapterCoverage")
			for _, s := range missing {
				fmt.Println("    " + s)
			}
			fmt.Println("FAIL")
			code = 1
		} else if testing.Verbose() {
			fmt.Println("--- PASS: TestAdapterCoverage")
		}
	}
	os.Exit(code)
}

func wholeRun() bool {
	for _, f := range []string{"test.run", "test.skip", "test.list"} {
		if fl := flag.Lookup(f); fl != nil && fl.Value.String() != "" {
			return false
		}
	}
	return true
}

// adapterCoverage: each exported method of an exported coreadapt type
// (read from the package's own sources) has been paired with a golden by
// some test; else a line naming it.
func adapterCoverage() []string {
	methods, err := adapterMethods(".")
	if err != nil {
		return []string{"reading the adapters: " + err.Error()}
	}
	replays.Lock()
	defer replays.Unlock()
	var missing []string
	for _, m := range methods {
		if len(replays.pairs[m]) == 0 {
			missing = append(missing, m+": no test replays a golden through it (use replay(t), replay_test.go)")
		}
	}
	return missing
}

// adapterMethods lists "Type.Method" of the exported methods with an
// exported receiver in dir's non-test Go files, sorted.
func adapterMethods(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 || !fd.Name.IsExported() {
					continue
				}
				typ := fd.Recv.List[0].Type
				if s, ok := typ.(*ast.StarExpr); ok {
					typ = s.X
				}
				id, ok := typ.(*ast.Ident)
				if !ok || !id.IsExported() {
					continue
				}
				out = append(out, id.Name+"."+fd.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// The guard's own pieces: the frame names it pairs and the methods it lists.
func TestAdapterCoverageParts(t *testing.T) {
	for fn, want := range map[string]string{
		adaptPkg + "Library.Item":                                  "Library.Item",
		adaptPkg + "(*Profiles).Languages":                         "Profiles.Languages",
		adaptPkg + "Library.SeriesFolder.func1":                    "Library.SeriesFolder",
		adaptPkg + "Forward":                                       "",
		adaptPkg[:len(adaptPkg)-1] + "_test.TestX":                 "",
		"github.com/antresol-app/antresol/addonkit.(*Client).Item": "",
	} {
		got, _ := adapterMethod(fn)
		if got != want {
			t.Errorf("%s: %q, want %q", fn, got, want)
		}
	}
	ms, err := adapterMethods(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Library.Item", "Profiles.Languages", "Tracking.Kick", "Images.Fetch"} {
		if !contains(ms, want) {
			t.Errorf("adapter methods %v lack %s", ms, want)
		}
	}
	for _, m := range ms {
		if strings.HasSuffix(m, ".users") || strings.HasPrefix(m, "Finished.") {
			t.Errorf("listed %s", m)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
