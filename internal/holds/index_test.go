package holds

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// A finished, parked download inside a file root: one hold keyed by the
// lower-case infohash, every file listed (the unselected one too), not
// active, the item from itemOf, the warnings never empty.
func TestBuild_Holds(t *testing.T) {
	item := int64(42)
	list := []Info{{
		Status: torrent.Status{InfoHash: "ABCDEF", Name: "Альбом", SaveDir: "/m/torrents", Mode: torrent.ModeDownload,
			Status: torrent.StatusSeeding, Parked: true, ItemID: &item},
		Files: []torrent.FileInfo{
			{Index: 0, FullPath: "/m/torrents/Альбом/CD1/01.flac", Size: 5, Selected: true},
			{Index: 1, FullPath: "/m/torrents/Альбом/CD2/02.flac", Size: 3},
		},
	}}
	var asked []string
	itemOf := func(ih string) int64 { asked = append(asked, ih); return 7 }
	got, outside := Build(list, func(ih string) bool { return ih == "abcdef" }, itemOf, []string{"/m"})
	want := []addonkit.Hold{{
		Key: "abcdef", Root: "/m/torrents/Альбом", Active: false, ItemID: 7, Busy: true,
		Files: []addonkit.HoldFile{
			{Path: "/m/torrents/Альбом/CD1/01.flac", Size: 5, Selected: true},
			{Path: "/m/torrents/Альбом/CD2/02.flac", Size: 3, Selected: false},
		},
		Label:       addonkit.Text{RU: "Загрузка «Альбом»", EN: "Download “Альбом”"},
		MoveWarning: addonkit.Text{RU: "Раздача «{name}» прекратится", EN: "Seeding of “{name}” will stop"},
		KeptWarning: addonkit.Text{RU: "Раздача «{name}» продолжится: в ней есть и другое из медиатеки", EN: "Seeding of “{name}” will go on: it also holds other things in the library"},
	}}
	if outside != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("outside %d\n got %+v\nwant %+v", outside, got, want)
	}
	if !reflect.DeepEqual(asked, []string{"abcdef"}) {
		t.Fatalf("itemOf asked %v", asked)
	}
	// running and not parked → active; paused → not
	list[0].Status.Parked = false
	if h, _ := Build(list, nil, nil, []string{"/m"}); !h[0].Active || h[0].ItemID != 0 || h[0].Busy {
		t.Fatalf("running: %+v", h[0])
	}
	list[0].Status.Status = torrent.StatusPaused
	if h, _ := Build(list, nil, nil, []string{"/m"}); h[0].Active {
		t.Fatal("paused is active")
	}
	// no metadata yet: no hold, not counted
	if h, n := Build([]Info{{Status: torrent.Status{InfoHash: "cc", SaveDir: "/m/torrents"}}}, nil, nil, []string{"/m"}); len(h) != 0 || n != 0 || h == nil {
		t.Fatalf("no metadata: %v %d", h, n)
	}
}

// The live layout: the downloads dir is under no file root, so its holds
// are left out and counted; a file root that is a symlink to the
// downloads dir, or a save dir reached through a symlink into a root,
// keeps them.
func TestBuild_OutsideFileRoots(t *testing.T) {
	live := []Info{{Status: torrent.Status{InfoHash: "aa", Name: "A", SaveDir: "/media/.torrents", Status: torrent.StatusSeeding}}}
	h, outside := Build(live, nil, nil, []string{"/data", "/media/movies", "/media/series"})
	if len(h) != 0 || outside != 1 {
		t.Fatalf("live layout: %v outside %d", h, outside)
	}
	// stream torrents outside are not counted (the stream cache's own)
	stream := []Info{{Status: torrent.Status{InfoHash: "bb", Name: "bb", SaveDir: "/tmp/vault-stream", Mode: torrent.ModeStream}}}
	if h, outside := Build(stream, nil, nil, []string{"/data"}); len(h) != 0 || outside != 0 {
		t.Fatalf("stream: %v %d", h, outside)
	}

	dir := t.TempDir()
	dl := filepath.Join(dir, "torrents")
	mustMkdir(t, filepath.Join(dl, "A"))
	link := filepath.Join(dir, "downloads")
	if err := os.Symlink(dl, link); err != nil {
		t.Fatal(err)
	}
	list := []Info{{Status: torrent.Status{InfoHash: "aa", Name: "A", SaveDir: dl, Status: torrent.StatusSeeding}}}
	if h, outside := Build(list, nil, nil, []string{link}); len(h) != 1 || outside != 0 || h[0].Root != filepath.Join(dl, "A") {
		t.Fatalf("root symlinked to the downloads: %v %d", h, outside)
	}
	resolved, _ := filepath.EvalSymlinks(dl)
	if h, outside := Build(list, nil, nil, []string{resolved}); len(h) != 1 || outside != 0 {
		t.Fatalf("resolved root: %v %d", h, outside)
	}
	// the save dir through a symlink, the root resolved (as /info has it)
	list[0].Status.SaveDir = link
	if h, outside := Build(list, nil, nil, []string{resolved}); len(h) != 1 || outside != 0 || h[0].Root != filepath.Join(link, "A") {
		t.Fatalf("save dir through a link: %v %d", h, outside)
	}
	// a download not on disk yet inside a root
	list[0].Status.SaveDir, list[0].Status.Name = filepath.Join(link, "new"), "B"
	if h, outside := Build(list, nil, nil, []string{resolved}); len(h) != 1 || outside != 0 {
		t.Fatalf("not on disk yet: %v %d", h, outside)
	}
	// a symlink inside a root pointing out of it: outside
	other := t.TempDir()
	mustMkdir(t, filepath.Join(other, "A"))
	if err := os.Symlink(other, filepath.Join(dl, "out")); err != nil {
		t.Fatal(err)
	}
	list[0].Status.SaveDir, list[0].Status.Name = filepath.Join(dl, "out"), "A"
	if h, outside := Build(list, nil, nil, []string{resolved}); len(h) != 0 || outside != 1 {
		t.Fatalf("link out of the root: %v %d", h, outside)
	}
}

// A hold under a root missing on disk (/info's missingRoots: a mount not up
// yet) is inside: kept in the set, never counted outside.
func TestBuild_MissingRootKept(t *testing.T) {
	list := []Info{
		{Status: torrent.Status{InfoHash: "aa", Name: "A", SaveDir: "/media/.torrents", Status: torrent.StatusSeeding}},
		{Status: torrent.Status{InfoHash: "bb", Name: "B", SaveDir: "/elsewhere", Status: torrent.StatusSeeding}},
	}
	h, outside := buildIn(list, nil, nil, []string{"/data"}, []string{"/media/.torrents"})
	if len(h) != 1 || h[0].Key != "aa" || h[0].Root != "/media/.torrents/A" || outside != 1 {
		t.Fatalf("%+v outside %d", h, outside)
	}
	// the missing root is matched as spelled, not as a prefix
	if h, outside := buildIn(list[:1], nil, nil, nil, []string{"/media/.tor"}); len(h) != 0 || outside != 1 {
		t.Fatalf("prefix: %+v %d", h, outside)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
