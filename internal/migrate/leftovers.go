package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
	"github.com/antresol-app/antresol/addonkit"
)

// LeftoverPaths are the files of what the migration cannot carry over:
// Librarr's lists, Seerr's pending requests, qBittorrent's RSS rules and
// watched folders. (Jellyfin's TV channels are the core import's.)
type LeftoverPaths struct {
	LibrarrDB, SeerrDB, QBRSSRules, QBWatched string
}

// Line.Source of leftovers that belong to no importer.
const (
	sourceLibrarr = "librarr"
	sourceSeerr   = "seerr"
)

// Leftovers lists, as warnings (Kind "leftover"), what stays behind and
// how much of it: nothing is lost silently. It only reads — databases
// through a snapshot in tmp. A missing file or table, or nothing pending,
// gives no line; a file that cannot be read gives a warning to check it by
// hand.
func Leftovers(ctx context.Context, p LeftoverPaths, tmp string) []Line {
	lang := i18n.FromContext(ctx)
	var out []Line
	warn := func(source, name, reason string) {
		out = append(out, Line{Source: source, Kind: "leftover", Name: name, Status: StatusWarning, Reason: reason})
	}
	unreadable := func(source, name string, err error) {
		arrLog().Warn("leftovers", "source", source, "err", err)
		warn(source, name, i18n.TL(lang, "Не удалось проверить %s — посмотрите там вручную, что не переносится", name))
	}

	if n, err := mediaimport.CountIn(ctx, tmp, p.LibrarrDB, mediaimport.TableCount{Table: "wishlist"}, mediaimport.TableCount{Table: "monitored_authors"},
		mediaimport.TableCount{Table: "series_tracking"}, mediaimport.TableCount{Table: "requests", Where: "status='pending'"}); err != nil {
		unreadable(sourceLibrarr, "Librarr", err)
	} else if n > 0 {
		warn(sourceLibrarr, "Librarr", i18n.TL(lang, "Не переносится: книги из списков Librarr (%d) — добавьте вручную", n))
	}

	if n, err := mediaimport.CountIn(ctx, tmp, p.SeerrDB, mediaimport.TableCount{Table: "media_request", Where: "status=1"}); err != nil {
		unreadable(sourceSeerr, "Seerr", err)
	} else if n > 0 {
		warn(sourceSeerr, "Seerr", i18n.TL(lang, "Не переносится: неодобренные запросы Seerr (%d)", n))
	}

	if n, err := enabledRSSRules(p.QBRSSRules); err != nil {
		unreadable(SourceQBittorrent, i18n.TL(lang, "правила RSS qBittorrent"), err)
	} else if n > 0 {
		warn(SourceQBittorrent, i18n.TL(lang, "Правила RSS"), i18n.TL(lang, "Не переносится: правила RSS qBittorrent (%d)", n))
	}

	if n, err := watchedFolders(p.QBWatched); err != nil {
		unreadable(SourceQBittorrent, i18n.TL(lang, "следимые папки qBittorrent"), err)
	} else if n > 0 {
		warn(SourceQBittorrent, i18n.TL(lang, "Следимые папки"), i18n.TL(lang, "Не переносится: следимые папки qBittorrent (%d)", n))
	}

	return out
}

// readJSON reads a small JSON file of qBittorrent (false: no file or an
// empty one).
func readJSON(path string, v any) (bool, error) {
	if mediaimport.Empty(path) {
		return false, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// enabledRSSRules counts the rules of rss/download_rules.json with
// "enabled": true.
func enabledRSSRules(path string) (int, error) {
	var rules map[string]struct {
		Enabled bool `json:"enabled"`
	}
	if ok, err := readJSON(path, &rules); !ok || err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rules {
		if r.Enabled {
			n++
		}
	}
	return n, nil
}

// watchedFolders counts the folders of watched_folders.json (an object
// keyed by folder; older versions wrote a list).
func watchedFolders(path string) (int, error) {
	var v any
	if ok, err := readJSON(path, &v); !ok || err != nil {
		return 0, err
	}
	switch w := v.(type) {
	case map[string]any:
		return len(w), nil
	case []any:
		return len(w), nil
	case nil:
		return 0, nil
	}
	return 0, errors.New("watched_folders.json: unexpected shape")
}

// LeftoverLines is Leftovers of the service's old services, in ctx's
// language, read through snapshots in a run folder of its own (removed
// before it returns). It is the screen's view: the addon's «Переезд» page
// (GET /api/admin/migrate) and the core's (migrate.leftovers).
func (s *Service) LeftoverLines(ctx context.Context) []Line {
	tmp, err := s.RunTmp()
	if err != nil {
		s.Logger().Warn("leftovers: temporary folder", "err", err)
		return []Line{}
	}
	defer os.RemoveAll(tmp)
	out := Leftovers(ctx, s.Leftover, tmp)
	if out == nil {
		out = []Line{}
	}
	return out
}

// LeftoversHook answers migrate.leftovers: the body is {"lang"}
// (addonkit.LeftoversRequest; "" is Russian) and the lines come back in
// that language. The core caches the answer (60 s) and puts the lines
// before its own on its «Переезд» screen.
func (s *Service) LeftoversHook(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.LeftoversRequest
	if len(in) > 0 {
		if err := json.Unmarshal(in, &req); err != nil {
			return nil, err
		}
	}
	lang := req.Lang
	if lang == "" {
		lang = i18n.RU
	}
	lines := s.LeftoverLines(i18n.WithLanguage(ctx, lang))
	out := addonkit.LeftoversAnswer{Lines: make([]addonkit.LeftoverLine, 0, len(lines))}
	for _, l := range lines {
		out.Lines = append(out.Lines, addonkit.LeftoverLine{Source: l.Source, Name: l.Name, Status: l.Status, Reason: l.Reason})
	}
	return out, nil
}
