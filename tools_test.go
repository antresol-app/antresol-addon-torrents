package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const copiedHeader = "// Code copied from the core by tools/corecopy.sh. DO NOT EDIT.\n"

// coreDir is the core checkout corecopy reads: $ANTRESOL_CORE, else the
// core repository this module sits in (git's top level holding addonkit/),
// else "" (none).
func coreDir() string {
	if d := os.Getenv("ANTRESOL_CORE"); d != "" {
		return d
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(top, "addonkit", "go.mod")); err != nil {
		return ""
	}
	return top
}

// TestCorecopyInSync: internal/corecopy is what tools/corecopy.sh makes of
// the core checkout today.
func TestCorecopyInSync(t *testing.T) {
	core := coreDir()
	if core == "" {
		t.Skip("no core checkout (ANTRESOL_CORE)")
	}
	if fi, err := os.Stat(filepath.Join(core, "internal")); err != nil || !fi.IsDir() {
		t.Skipf("no core checkout at %s (ANTRESOL_CORE)", core)
	}
	out, err := exec.Command("sh", "tools/corecopy.sh", "--check", core).CombinedOutput()
	if err != nil {
		t.Fatalf("corecopy --check: %v\n%s", err, out)
	}
}

// fakeModule makes a module dir with this tools/corecopy.sh and the given
// list, and a core dir with the given internal/ files.
func fakeModule(t *testing.T, list string, core map[string]string) (mod, coreRoot string) {
	t.Helper()
	root := t.TempDir()
	mod, coreRoot = filepath.Join(root, "mod"), filepath.Join(root, "core")
	script, err := os.ReadFile("tools/corecopy.sh")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(mod, "tools/corecopy.sh"), string(script))
	write(t, filepath.Join(mod, "tools/corecopy.list"), list)
	for name, body := range core {
		write(t, filepath.Join(coreRoot, "internal", name), body)
	}
	return mod, coreRoot
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func corecopy(t *testing.T, mod string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(mod, "tools/corecopy.sh")}, args...)...)
	cmd.Dir = t.TempDir() // the script must not depend on the caller's directory
	cmd.Env = append(os.Environ(), "ANTRESOL_CORE=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCorecopyClosure(t *testing.T) {
	mod, core := fakeModule(t, "a/*.go\n", map[string]string{
		"a/a.go": "package a\n\nimport \"github.com/antresol-app/antresol/internal/b\"\n\nvar X = b.Y\n",
		"b/b.go": "package b\n\nvar Y = 1\n",
	})
	out, err := corecopy(t, mod, core)
	if err == nil || !strings.Contains(out, "internal/b") {
		t.Fatalf("want a closure failure naming internal/b, got %v:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(mod, "internal/corecopy")); !os.IsNotExist(err) {
		t.Fatalf("a refused copy wrote internal/corecopy: %v", err)
	}
}

func TestCorecopyDropsSourcesTag(t *testing.T) {
	mod, core := fakeModule(t, "# helpers\na/*.go b/b.go\n", map[string]string{
		"a/a.go":      "//go:build sources\n\n// Package a is a.\npackage a\n\nimport \"github.com/antresol-app/antresol/internal/b\"\n\nvar X = b.Y\n",
		"a/a_test.go": "package a\n",
		"a/u_unix.go": "//go:build unix\n\npackage a\n",
		"b/b.go":      "package b\n\nvar Y = 1\n",
	})
	if out, err := corecopy(t, mod, core); err != nil {
		t.Fatalf("corecopy: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(mod, "internal/corecopy/a/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := copiedHeader + "\n// Package a is a.\npackage a\n\nimport \"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/b\"\n\nvar X = b.Y\n"
	if string(got) != want {
		t.Fatalf("a.go:\n%s\nwant:\n%s", got, want)
	}
	if u, _ := os.ReadFile(filepath.Join(mod, "internal/corecopy/a/u_unix.go")); string(u) != copiedHeader+"\n//go:build unix\n\npackage a\n" {
		t.Fatalf("u_unix.go keeps its other build line:\n%s", u)
	}
	if _, err := os.Stat(filepath.Join(mod, "internal/corecopy/a/a_test.go")); !os.IsNotExist(err) {
		t.Fatalf("a test file was copied: %v", err)
	}
	if out, err := corecopy(t, mod, "--check", core); err != nil {
		t.Fatalf("--check right after a copy: %v\n%s", err, out)
	}
	// drift: the core changes, the copy does not
	write(t, filepath.Join(core, "internal/b/b.go"), "package b\n\nvar Y = 2\n")
	if out, err := corecopy(t, mod, "--check", core); err == nil || !strings.Contains(out, "b/b.go") {
		t.Fatalf("--check after a core change: want a failure naming b/b.go, got %v:\n%s", err, out)
	}
}

// A file the sources build leaves out (//go:build !sources) or a build line
// that only partly names sources is refused: it needs a conscious choice.
func TestCorecopyRefusesOtherSourcesLines(t *testing.T) {
	for _, line := range []string{"//go:build !sources", "//go:build sources && unix"} {
		mod, core := fakeModule(t, "a/*.go\n", map[string]string{"a/a.go": line + "\n\npackage a\n"})
		if out, err := corecopy(t, mod, core); err == nil || !strings.Contains(out, "a/a.go") {
			t.Fatalf("%s: want a refusal naming a/a.go, got %v:\n%s", line, err, out)
		}
	}
}

// Files the family alone named in the core are never copied: a glob passes
// over them, naming one is refused.
func TestCorecopyFamilyOnlyFiles(t *testing.T) {
	core := map[string]string{
		"events/bus.go":           "package events\n",
		"events/types_full.go":    "//go:build sources\n\npackage events\n",
		"library/model.go":        "package library\n",
		"library/torrent_full.go": "//go:build sources\n\npackage library\n",
	}
	mod, root := fakeModule(t, "events/*.go library/*.go\n", core)
	if out, err := corecopy(t, mod, root); err != nil {
		t.Fatalf("globs: %v\n%s", err, out)
	}
	for _, f := range []string{"events/types_full.go", "library/torrent_full.go"} {
		if _, err := os.Stat(filepath.Join(mod, "internal/corecopy", f)); !os.IsNotExist(err) {
			t.Fatalf("%s copied: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mod, "internal/corecopy/events/bus.go")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"events/types_full.go", "library/torrent_full.go"} {
		mod, root := fakeModule(t, "events/bus.go library/model.go "+f+"\n", core)
		if out, err := corecopy(t, mod, root); err == nil || !strings.Contains(out, f) {
			t.Fatalf("naming %s: want a refusal, got %v:\n%s", f, err, out)
		}
	}
}

func TestCorecopyGlobMatchingNothing(t *testing.T) {
	mod, core := fakeModule(t, "a/*.go nope/*.go\n", map[string]string{"a/a.go": "package a\n"})
	if out, err := corecopy(t, mod, core); err == nil || !strings.Contains(out, "nope/*.go") {
		t.Fatalf("want a failure naming nope/*.go, got %v:\n%s", err, out)
	}
}

// TestFence: the files fenced with //go:build ignore are exactly those
// tools/fenced.txt lists.
func TestFence(t *testing.T) {
	if missing, extra, err := fenceDiff(".", "tools/fenced.txt"); err != nil || len(missing)+len(extra) > 0 {
		t.Fatalf("fence: err %v; fenced but not listed %v; listed but not fenced %v", err, missing, extra)
	}
}

// TestFenceOnlyMusic: only the music import is fenced (its
// addon side waits for a later stage) — internal/musicimport and its legacy
// handler with its test; legacy/ holds only those and the one migration.
// The later stage that wires it deletes legacy/ and this test.
func TestFenceOnlyMusic(t *testing.T) {
	raw, err := os.ReadFile("tools/fenced.txt")
	if err != nil {
		t.Fatal(err)
	}
	legacyMusic := map[string]bool{
		"legacy/api/import_handlers_full.go":       true,
		"legacy/api/import_handlers_moved_test.go": true,
	}
	var other []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !strings.HasPrefix(l, "internal/musicimport/") && !legacyMusic[l] {
			other = append(other, l)
		}
	}
	if len(other) > 0 {
		t.Fatalf("fenced beyond the music import: %v", other)
	}
	err = filepath.WalkDir("legacy", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if p := filepath.ToSlash(path); !legacyMusic[p] && p != "legacy/migrations/0038_item_owners_backfill.sql" {
			t.Errorf("%s: legacy/ keeps only the music import's handlers and 0038", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFenceDetects(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "tools/fenced.txt"), "# fenced files\n")
	write(t, filepath.Join(root, "x/x.go"), "package x\n\nconst s = `\n//go:build ignore\n`\n")
	if missing, extra, err := fenceDiff(root, filepath.Join(root, "tools/fenced.txt")); err != nil || len(missing)+len(extra) > 0 {
		t.Fatalf("no fence: %v %v %v", err, missing, extra)
	}
	write(t, filepath.Join(root, "x/y.go"), "// y waits for its sub-plan\n\n//go:build ignore\n\npackage x\n")
	if missing, _, _ := fenceDiff(root, filepath.Join(root, "tools/fenced.txt")); !slices.Equal(missing, []string{"x/y.go"}) {
		t.Fatalf("an unlisted fenced file: %v", missing)
	}
	write(t, filepath.Join(root, "tools/fenced.txt"), "x/y.go\nx/z.go\n")
	if missing, extra, _ := fenceDiff(root, filepath.Join(root, "tools/fenced.txt")); len(missing) != 0 || !slices.Equal(extra, []string{"x/z.go"}) {
		t.Fatalf("a listed file not fenced: %v %v", missing, extra)
	}
}

// fenceDiff compares the .go files under root whose build line (in the
// header, before the package clause) is //go:build ignore with the list
// (one slash path relative to root per line, # comments).
func fenceDiff(root, list string) (missing, extra []string, err error) {
	raw, err := os.ReadFile(list)
	if err != nil {
		return nil, nil, err
	}
	listed := map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			listed[l] = true
		}
	}
	fenced := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (n == "node_modules" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		ok, err := hasIgnoreFence(p)
		if ok {
			rel, _ := filepath.Rel(root, p)
			fenced[filepath.ToSlash(rel)] = true
		}
		return err
	})
	for f := range fenced {
		if !listed[f] {
			missing = append(missing, f)
		}
	}
	for f := range listed {
		if !fenced[f] {
			extra = append(extra, f)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra, err
}

func hasIgnoreFence(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "//go:build ignore":
			return true, nil
		case l == "" || strings.HasPrefix(l, "//"):
		default: // the package clause or a block comment: the header is over
			return false, nil
		}
	}
	return false, sc.Err()
}
