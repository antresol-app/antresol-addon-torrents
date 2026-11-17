package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// coreTableSQL is SQL that reads or writes one of the core's tables: after
// FROM, JOIN, INTO or UPDATE (UPDATE OR IGNORE …), the table itself or one
// of a comma list (FROM acquisitions a, items i), maybe quoted or as
// main.<table>.
var coreTableSQL = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE(?:\s+OR\s+\w+)?)\s+` +
	`(?:[\w."` + "`" + `]+(?:\s+(?:AS\s+)?\w+)?\s*,\s*)*` +
	`["` + "`" + `\[]?(?:main\.)?["` + "`" + `\[]?` +
	`(items|item_titles|files|users|series_tracks|tracked_titles|title_marks|item_owners|online_progress|import_groups|import_tracks|` +
	// the core's tracking, fit and letters tables (internal/db/migrations), which the addon no longer touches
	`episode_sources|title_episodes|title_choices|tracked_scans|external_audio_fit|watchlist|kid_requests|` +
	`notify_addon_prefs|notify_events|notify_prefs|notify_seen|notify_state|notify_verify_log)\b`)

// sqlGuarded are the packages that reach the core only through addonkit.
var sqlGuarded = []string{"acquire", "indexer", "torrent", "organize", "admin", "holds", "importer", "boot", "watch", "coreadapt", "find", "libraryhooks", "follow", "pinned", "followapi", "trackinghooks", "studios", "migrate", "migrateapi"}

// sqlAllowed are the only core-table literals a guarded package may hold,
// by file (relative to internal/): the importer's reads of its snapshot of
// vault.db, and its test's write into the source it snapshots. Each entry
// must still be there (a stale one fails).
var sqlAllowed = map[string][]string{
	"importer/copyrows.go": {
		"SELECT path, lower(torrent_infohash) FROM files WHERE torrent_infohash IS NOT NULL AND torrent_infohash != ''",
	},
	"importer/phase1_test.go": {
		"INSERT INTO items (id, kind, title, year) VALUES (9, 'movie', 'Core change', 2020)",
		"DELETE FROM users WHERE id=2", // a profile delete in the source (FK SET NULL)
	},
}

// The addon never queries a core table: no string of a guarded package
// (each literal, and each +-chain of literals folded into one; tests
// included; //go:build ignore files, which do not build, left out) names
// one in SQL, but for sqlAllowed.
func TestNoCoreTableSQL(t *testing.T) {
	walked := 0
	seen := map[string]bool{} // file + "\x00" + literal of sqlAllowed met
	for _, pkg := range sqlGuarded {
		root := filepath.Join("internal", pkg)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue // not written yet
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			if ignored(f) {
				return nil
			}
			walked++
			rel := filepath.ToSlash(strings.TrimPrefix(p, "internal"+string(filepath.Separator)))
			check := func(pos token.Pos, s string) {
				m := coreTableSQL.FindString(s)
				if m == "" {
					return
				}
				for _, ok := range sqlAllowed[rel] {
					if s == ok {
						seen[rel+"\x00"+s] = true
						return
					}
				}
				t.Errorf("%s: SQL on a core table (%q): use the core's API (coreadapt)", fset.Position(pos), m)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BinaryExpr:
					if x.Op == token.ADD {
						if s, ok := folded(x); ok {
							check(x.Pos(), s)
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						check(x.Pos(), unquote(x))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if walked == 0 {
		t.Fatal("no Go files walked")
	}
	for file, lits := range sqlAllowed {
		for _, s := range lits {
			if !seen[file+"\x00"+s] {
				t.Errorf("sqlAllowed: %s no longer holds %q: drop the entry", file, s)
			}
		}
	}
}

// folded is a +-chain with a string literal in it as one string, other
// operands as "?" (so "FROM " + "items" and "FROM "+q+" JOIN items" are
// read whole); ok false for a chain without a string literal.
func folded(e *ast.BinaryExpr) (string, bool) {
	var b strings.Builder
	lit := false
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		switch v := x.(type) {
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				walk(v.X)
				walk(v.Y)
				return
			}
		case *ast.ParenExpr:
			walk(v.X)
			return
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				b.WriteString(unquote(v))
				lit = true
				return
			}
		}
		b.WriteString("?")
	}
	walk(e)
	return b.String(), lit
}

func unquote(lit *ast.BasicLit) string {
	if s, err := strconv.Unquote(lit.Value); err == nil {
		return s
	}
	return lit.Value
}

// ignored: the file starts with //go:build ignore.
func ignored(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.TrimSpace(c.Text) == "//go:build ignore" {
				return true
			}
		}
	}
	return false
}

// The guard's pattern catches the spellings a query may use, and not the
// addon's own tables.
func TestCoreTableSQLPattern(t *testing.T) {
	for _, s := range []string{
		"SELECT 1 FROM files", "UPDATE OR IGNORE files SET x=1", "SELECT * FROM main.items",
		`SELECT * FROM "items"`, "SELECT 1 FROM acquisitions a, items i WHERE a.x=i.id",
		"INSERT OR REPLACE INTO users(id) VALUES(1)", "x JOIN item_titles t ON 1",
		"SELECT * FROM episode_sources WHERE kind='pinned'", "UPDATE title_episodes SET watched=1", "SELECT 1 FROM tracked_scans",
		"INSERT INTO title_choices(user_id) VALUES(1)", "SELECT 1 FROM external_audio_fit", "SELECT 1 FROM notify_events",
	} {
		if !coreTableSQL.MatchString(s) {
			t.Errorf("not caught: %q", s)
		}
	}
	for _, s := range []string{
		"SELECT 1 FROM acquisitions", "UPDATE torrents SET x=1", "SELECT 1 FROM torrent_files",
		"INSERT INTO import_refs(path) VALUES(?)", "the files from the download",
	} {
		if coreTableSQL.MatchString(s) {
			t.Errorf("caught: %q", s)
		}
	}
}
