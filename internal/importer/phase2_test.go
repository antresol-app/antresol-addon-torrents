package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol/addonkit"
)

// fakeBinder answers BindRefs from its fields and records every call.
type fakeBinder struct {
	calls   [][]addonkit.RefPath
	missing map[string]bool
	taken   map[string]bool
	failN   int // the first failN calls fail
}

func (f *fakeBinder) BindRefs(_ context.Context, pairs []addonkit.RefPath) (addonkit.Bound, error) {
	f.calls = append(f.calls, slices.Clone(pairs))
	if len(f.calls) <= f.failN {
		return addonkit.Bound{}, errors.New("core: 503 Расширение запускается")
	}
	var b addonkit.Bound
	for _, p := range pairs {
		switch {
		case f.missing[p.Path]:
			b.Missing = append(b.Missing, p.Path)
		case f.taken[p.Path]:
			b.Taken = append(b.Taken, p.Path)
		default:
			b.Bound++
		}
	}
	return b, nil
}

// phase1DB is a torrents.db after phase 1 with n refs (paths /m/0 … /m/n-1).
func phase1DB(t *testing.T, n int) *sql.DB {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec(`INSERT INTO import_state (id, source, stamp, counts_json, phase, fingerprint, updated_at) VALUES (1, '/v/vault.db', 'now', '{}', 1, 'fp', 'now')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := d.Exec(`INSERT INTO import_refs (path, ref) VALUES (?, ?)`, fmt.Sprintf("/m/%d", i), fmt.Sprintf("%040d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func states(t *testing.T, d *sql.DB) map[string]string {
	t.Helper()
	rows, err := d.Query(`SELECT path, state FROM import_refs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			t.Fatal(err)
		}
		out[p] = s
	}
	return out
}

func phase(t *testing.T, d *sql.DB) int {
	t.Helper()
	st, ok, err := LoadState(context.Background(), d)
	if err != nil || !ok {
		t.Fatalf("LoadState: %v %v", ok, err)
	}
	return st.Phase
}

func TestPhase2_BindsAll(t *testing.T) {
	d := phase1DB(t, 4)
	b := &fakeBinder{}
	done, err := Phase2(context.Background(), d, b, 0)
	if err != nil || !done {
		t.Fatalf("Phase2 = %v, %v", done, err)
	}
	if len(b.calls) != 1 || len(b.calls[0]) != 4 {
		t.Fatalf("calls %v", b.calls)
	}
	if b.calls[0][0].Ref != fmt.Sprintf("%040d", 0) || b.calls[0][0].Path != "/m/0" {
		t.Fatalf("pair %+v", b.calls[0][0])
	}
	for p, s := range states(t, d) {
		if s != "bound" {
			t.Errorf("%s %s", p, s)
		}
	}
	if phase(t, d) != 2 {
		t.Fatal("phase not 2")
	}
	// done: a rerun calls nothing
	if done, err := Phase2(context.Background(), d, b, 0); err != nil || !done || len(b.calls) != 1 {
		t.Fatalf("rerun: %v %v, %d calls", done, err, len(b.calls))
	}
}

func TestPhase2_MissingAndTaken(t *testing.T) {
	d := phase1DB(t, 4)
	b := &fakeBinder{missing: map[string]bool{"/m/1": true}, taken: map[string]bool{"/m/2": true}}
	if done, err := Phase2(context.Background(), d, b, 0); err != nil || !done {
		t.Fatalf("Phase2 = %v, %v", done, err)
	}
	want := map[string]string{"/m/0": "bound", "/m/1": "missing", "/m/2": "taken", "/m/3": "bound"}
	if got := states(t, d); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("states %v, want %v", got, want)
	}
	if phase(t, d) != 2 {
		t.Fatal("phase not 2")
	}
}

func TestPhase2_ResumesAfterError(t *testing.T) {
	d := phase1DB(t, 4)
	b := &fakeBinder{failN: 1}
	done, err := Phase2(context.Background(), d, b, 0)
	if err == nil || done {
		t.Fatalf("first run = %v, %v; want an error", done, err)
	}
	for p, s := range states(t, d) {
		if s != "pending" {
			t.Errorf("%s %s after a failed call", p, s)
		}
	}
	st, _, _ := LoadState(context.Background(), d)
	if st.Phase != 1 || st.Error == "" {
		t.Fatalf("state after the failure: %+v", st)
	}
	if done, err := Phase2(context.Background(), d, b, 0); err != nil || !done {
		t.Fatalf("second run = %v, %v", done, err)
	}
	if len(b.calls) != 2 {
		t.Fatalf("%d calls, want 2", len(b.calls))
	}
	st, _, _ = LoadState(context.Background(), d)
	if st.Phase != 2 || st.Error != "" {
		t.Fatalf("state after the retry: %+v", st)
	}
}

func TestPhase2_Batches(t *testing.T) {
	d := phase1DB(t, 5)
	b := &fakeBinder{}
	if done, err := Phase2(context.Background(), d, b, 2); err != nil || !done {
		t.Fatalf("Phase2 = %v, %v", done, err)
	}
	if len(b.calls) != 3 || len(b.calls[0]) != 2 || len(b.calls[2]) != 1 {
		t.Fatalf("calls %v", b.calls)
	}
	seen := map[string]bool{}
	for _, c := range b.calls {
		for _, p := range c {
			if seen[p.Path] {
				t.Fatalf("%s sent twice", p.Path)
			}
			seen[p.Path] = true
		}
	}
}

func TestPhase2_FailureMidwayKeepsBoundBatches(t *testing.T) {
	d := phase1DB(t, 5)
	b := &fakeBinder{}
	fail := &failSecond{b: b}
	if _, err := Phase2(context.Background(), d, fail, 2); err == nil {
		t.Fatal("no error")
	}
	got := states(t, d)
	if got["/m/0"] != "bound" || got["/m/1"] != "bound" || got["/m/2"] != "pending" || got["/m/4"] != "pending" {
		t.Fatalf("states %v", got)
	}
	// the rerun sends only the pending ones
	if done, err := Phase2(context.Background(), d, b, 2); err != nil || !done {
		t.Fatalf("rerun %v %v", done, err)
	}
	for _, c := range b.calls[1:] {
		for _, p := range c {
			if p.Path == "/m/0" || p.Path == "/m/1" {
				t.Fatalf("bound %s sent again", p.Path)
			}
		}
	}
}

type failSecond struct {
	b *fakeBinder
	n int
}

func (f *failSecond) BindRefs(ctx context.Context, pairs []addonkit.RefPath) (addonkit.Bound, error) {
	f.n++
	if f.n == 2 {
		return addonkit.Bound{}, errors.New("timeout")
	}
	return f.b.BindRefs(ctx, pairs)
}

func TestPhase2_NoImportNothingToDo(t *testing.T) {
	d, err := store.Open(filepath.Join(t.TempDir(), "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	b := &fakeBinder{}
	if done, err := Phase2(context.Background(), d, b, 0); err != nil || !done || len(b.calls) != 0 {
		t.Fatalf("no import_state: %v %v, %d calls", done, err, len(b.calls))
	}
}

func TestPhase2_ZeroRefs(t *testing.T) {
	d := phase1DB(t, 0)
	b := &fakeBinder{}
	if done, err := Phase2(context.Background(), d, b, 0); err != nil || !done || len(b.calls) != 0 {
		t.Fatalf("%v %v, %d calls", done, err, len(b.calls))
	}
	if phase(t, d) != 2 {
		t.Fatal("phase not 2 with nothing to bind")
	}
}
