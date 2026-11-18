package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/sqlitesnap"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// Line.Source of the Prowlarr, Sonarr and Radarr importers.
const (
	SourceProwlarr = "prowlarr"
	SourceSonarr   = follow.SourceSonarr
	SourceRadarr   = follow.SourceRadarr
)

// TMDBFinder resolves an external id (Sonarr's TVDB id) to a TMDB id; it is
// *tmdb.Client.
type TMDBFinder interface {
	Find(ctx context.Context, kind, source, extID string) (int, error)
}

// ProwlarrImporter is the part of *indexer.Manager the Prowlarr importer
// uses; tmpDir receives the database snapshot (removed before returning).
type ProwlarrImporter interface {
	PreviewProwlarrIn(ctx context.Context, path, tmpDir string) ([]indexer.Imported, error)
	ImportProwlarrIn(ctx context.Context, path, tmpDir string) ([]indexer.Imported, error)
}

// ArrOptions configure ImportProwlarr, ImportSonarr and ImportRadarr.
type ArrOptions struct {
	ProwlarrDB, SonarrDB, RadarrDB string
	UserID                         int64 // the admin running the migration: follows are theirs
	// UserUID is that admin's uid (X-Antresol-User uid, Runner.StartAs;
	// "" = unknown: the follows are nobody's).
	UserUID         string
	DryRun          bool
	FlareSolverrSet bool // ANTRESOL_TORRENTS_FLARESOLVERR_URL is set
	// Tmp is the run's private folder for database snapshots ("" = a
	// temporary folder of its own).
	Tmp string
	Now func() time.Time
}

func (o ArrOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

const (
	reasonFollowing  = "Уже отслеживается"
	reasonInLibrary  = "Уже в медиатеке"
	reasonNoTMDB     = "Не удалось найти в TMDB (нет ключа или сети)"
	reasonNoMovieID  = "В Radarr у фильма нет TMDB id"
	reasonSwitchOn   = "Включится кнопкой «Перейти на Antresol»"
	reasonSourceAdds = "Добавлен — проверьте его в «Источниках»"
)

func arrLog() *slog.Logger { return slog.Default().With("component", "migrate") }

// ImportProwlarr adds Prowlarr's trackers as Vault sources (the dry run
// only previews them). Indexers Prowlarr reaches through FlareSolverr get
// a warning while ANTRESOL_TORRENTS_FLARESOLVERR_URL is not set. A database that
// cannot be read is a failed line; the error is ctx's or an internal one.
func ImportProwlarr(ctx context.Context, o ArrOptions, m ProwlarrImporter, rep *Report) error {
	run := m.ImportProwlarrIn
	if o.DryRun {
		run = m.PreviewProwlarrIn
	}
	if o.Tmp != "" {
		if err := os.MkdirAll(o.Tmp, 0o700); err != nil {
			return err
		}
	}
	list, err := run(ctx, o.ProwlarrDB, o.Tmp)
	for _, im := range list {
		l := Line{Source: SourceProwlarr, Kind: "source", Name: im.Name, Reason: im.Reason}
		usable := true
		switch {
		case im.Source != nil:
			l.Status, l.Reason, l.Ref = StatusImported, reasonSourceAdds, "source:"+strconv.FormatInt(im.Source.ID, 10)
			if im.Reason != "" { // an existing source got Prowlarr's session cookie
				l.Reason = im.Reason
			}
		case im.Reason == indexer.ReasonWillAdd, im.Reason == indexer.ReasonWillUpdateCookie:
			l.Status = StatusPlanned
		case im.Reason == indexer.ReasonAlready:
			l.Status = StatusSkipped
		case strings.HasPrefix(im.Reason, "Тип «"): // not a torrent tracker: nothing to carry over
			l.Status, usable = StatusWarning, false
		default:
			l.Status, usable = StatusFailed, false
		}
		rep.Add(l)
		if usable && im.NeedsFlareSolverr && !o.FlareSolverrSet {
			rep.Add(Line{Source: SourceProwlarr, Kind: "source", Name: im.Name, Status: StatusWarning,
				Reason: fmt.Sprintf("%s: нужен FlareSolverr — укажите его адрес в «Управление → Настройки загрузок»", im.Name)})
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ie *indexer.ImportError
		if errors.As(err, &ie) {
			rep.Add(Line{Source: SourceProwlarr, Kind: "source", Name: "Prowlarr", Status: StatusFailed, Reason: ie.Msg})
			return nil
		}
		return err
	}
	return nil
}

// arrDB is a read-only snapshot of a Sonarr or Radarr database.
type arrDB struct {
	*sql.DB
	app   string // Sonarr | Radarr
	files []string
	tmp   string // own temporary folder, removed on close
}

func (d *arrDB) close() {
	d.DB.Close()
	for _, f := range d.files {
		os.Remove(f)
	}
	if d.tmp != "" {
		os.RemoveAll(d.tmp)
	}
}

// openArr snapshots path into o.Tmp and opens the copy; a missing or
// unreadable database is a failed line (nil returned).
func openArr(o ArrOptions, app, source, path string, rep *Report) *arrDB {
	fail := func(reason string) *arrDB {
		rep.Add(Line{Source: source, Kind: "follow", Name: app, Status: StatusFailed, Reason: reason})
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return fail(fmt.Sprintf("База %s не найдена: %s", app, path))
	}
	d := &arrDB{app: app}
	dir := o.Tmp
	if dir == "" {
		t, err := os.MkdirTemp("", "vault-migrate-")
		if err != nil {
			return fail(fmt.Sprintf("Не удалось прочитать базу %s", app))
		}
		dir, d.tmp = t, t
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(fmt.Sprintf("Не удалось прочитать базу %s", app))
	}
	cleanup := func() {
		if d.tmp != "" {
			os.RemoveAll(d.tmp)
		}
	}
	snap, err := sqlitesnap.Snapshot(path, dir)
	if err != nil {
		cleanup()
		arrLog().Warn("snapshot", "app", app, "file", path, "err", err)
		return fail(fmt.Sprintf("Не удалось прочитать базу %s", app))
	}
	d.files = []string{snap, snap + "-wal", snap + "-shm"}
	db, err := sqlitesnap.OpenRO(snap)
	if err != nil {
		for _, f := range d.files {
			os.Remove(f)
		}
		cleanup()
		arrLog().Warn("open snapshot", "app", app, "file", path, "err", err)
		return fail(fmt.Sprintf("Не удалось прочитать базу %s", app))
	}
	d.DB = db
	return d
}

// readFailed reports a database whose tables are not what was expected.
func (d *arrDB) readFailed(source string, rep *Report, err error) {
	arrLog().Warn("read", "app", d.app, "err", err)
	rep.Add(Line{Source: source, Kind: "follow", Name: d.app, Status: StatusFailed,
		Reason: fmt.Sprintf("Не удалось прочитать базу %s", d.app)})
}

// warnRemoveCompleted: a download client of Sonarr/Radarr that removes
// completed downloads would delete the torrents qBittorrent stops for the
// move, with their files. A version without a DownloadClients table has no
// clients; a table that cannot be checked is a warning too — this advice
// must never vanish silently.
func (d *arrDB) warnRemoveCompleted(ctx context.Context, source string, rep *Report) {
	unknown := func(err error) {
		arrLog().Warn("read download clients", "app", d.app, "err", err)
		rep.Add(Line{Source: source, Kind: "follow", Name: d.app, Status: StatusWarning,
			Reason: fmt.Sprintf("%s: не удалось проверить, удаляет ли он завершённые загрузки — проверьте в настройках клиента загрузки до переноса раздач", d.app)})
	}
	ok, err := sqlitesnap.HasTable(ctx, d.DB, "DownloadClients")
	if err != nil {
		unknown(err)
		return
	}
	if !ok {
		return
	}
	cols, err := sqlitesnap.Columns(ctx, d.DB, "DownloadClients")
	if err != nil {
		unknown(err)
		return
	}
	for _, c := range []string{"name", "enable", "removecompleteddownloads"} {
		if !cols[c] {
			unknown(fmt.Errorf("DownloadClients has no column %s", c))
			return
		}
	}
	rows, err := d.QueryContext(ctx, `SELECT COALESCE(Name, '') FROM DownloadClients WHERE Enable=1 AND RemoveCompletedDownloads=1 ORDER BY Id`)
	if err != nil {
		unknown(err)
		return
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			unknown(err)
			return
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		unknown(err)
		return
	}
	for _, name := range names {
		rep.Add(Line{Source: source, Kind: "follow", Name: name, Status: StatusWarning,
			Reason: fmt.Sprintf("%s удаляет завершённые загрузки (%s) — остановите его до переноса раздач, иначе он удалит остановленные раздачи вместе с файлами в torrents/", d.app, name)})
	}
}

func titleYear(title string, year int) string {
	if year > 0 {
		return fmt.Sprintf("%s (%d)", title, year)
	}
	return title
}

// posterOf picks the poster from Sonarr/Radarr's Images JSON: its
// remoteUrl, else its url when that is absolute (a local /MediaCover path
// is useless to Vault).
func posterOf(images string) string {
	var list []struct {
		CoverType string `json:"coverType"`
		URL       string `json:"url"`
		RemoteURL string `json:"remoteUrl"`
	}
	if json.Unmarshal([]byte(images), &list) != nil {
		return ""
	}
	for _, im := range list {
		if !strings.EqualFold(im.CoverType, "poster") {
			continue
		}
		if im.RemoteURL != "" {
			return im.RemoteURL
		}
		if strings.HasPrefix(im.URL, "http") {
			return im.URL
		}
		return ""
	}
	return ""
}

// followed: the title already has a follow (any source).
func followed(ctx context.Context, vdb *sql.DB, kind string, tmdbID int) (bool, error) {
	var one int
	err := vdb.QueryRowContext(ctx, `SELECT 1 FROM follows WHERE kind=? AND tmdb_id=?`, kind, tmdbID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// addFollow creates the inactive follow (or plans it in the dry run) and
// reports it.
func addFollow(ctx context.Context, o ArrOptions, vdb *sql.DB, rep *Report, in follow.ImportFollow, l Line, reason string) error {
	l.Ref = in.Kind + ":" + strconv.Itoa(in.TmdbID)
	has, err := followed(ctx, vdb, in.Kind, in.TmdbID)
	if err != nil {
		return err
	}
	if has {
		l.Status, l.Reason = StatusSkipped, reasonFollowing
		rep.Add(l)
		return nil
	}
	l.Reason = reason
	if o.DryRun {
		l.Status = StatusPlanned
		rep.Add(l)
		return nil
	}
	created, err := follow.ImportInactive(ctx, vdb, addonkit.User{ID: o.UserID, UID: o.UserUID}, in)
	if err != nil {
		return err
	}
	l.Status = StatusImported
	if !created {
		l.Status, l.Reason = StatusSkipped, reasonFollowing
	}
	rep.Add(l)
	return nil
}

type sonarrSeries struct {
	id, year, tvdb, tmdb int
	title, images        string
}

// ImportSonarr takes over Sonarr's monitored series as inactive follows
// (switched on by «Перейти на Vault»), each remembering the episodes
// Sonarr monitored and lacked: older episodes it was not told to get are
// not downloaded. Sonarr's database is only read, from a snapshot. tm may
// be nil (no TMDB key): series without a TMDB id then fail. The error is
// ctx's or Vault's database's.
func ImportSonarr(ctx context.Context, o ArrOptions, vdb *sql.DB, tm TMDBFinder, rep *Report) error {
	d := openArr(o, "Sonarr", SourceSonarr, o.SonarrDB, rep)
	if d == nil {
		return ctx.Err()
	}
	defer d.close()
	d.warnRemoveCompleted(ctx, SourceSonarr, rep)

	rows, err := d.QueryContext(ctx, `SELECT Id, COALESCE(Title, ''), COALESCE(Year, 0), COALESCE(TvdbId, 0), COALESCE(TmdbId, 0), COALESCE(Images, '[]')
		FROM Series WHERE Monitored=1 ORDER BY Id`)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.readFailed(SourceSonarr, rep, err)
		return nil
	}
	var list []sonarrSeries
	for rows.Next() {
		var s sonarrSeries
		if err := rows.Scan(&s.id, &s.title, &s.year, &s.tvdb, &s.tmdb, &s.images); err != nil {
			rows.Close()
			d.readFailed(SourceSonarr, rep, err)
			return nil
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		d.readFailed(SourceSonarr, rep, err)
		return nil
	}

	baseline := o.now().UTC()
	for _, s := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		l := Line{Source: SourceSonarr, Kind: "follow", Name: titleYear(s.title, s.year)}
		tmdbID := s.tmdb
		if tmdbID <= 0 && tm != nil && s.tvdb > 0 {
			id, err := tm.Find(ctx, "tv", "tvdb_id", strconv.Itoa(s.tvdb))
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				tmdbID = id
			}
		}
		if tmdbID <= 0 {
			l.Status, l.Reason = StatusFailed, reasonNoTMDB
			rep.Add(l)
			continue
		}
		want, err := sonarrWanted(ctx, d, s.id)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			arrLog().Warn("read episodes", "series", s.title, "err", err)
			l.Status, l.Reason = StatusFailed, "Не удалось прочитать серии в базе Sonarr"
			rep.Add(l)
			continue
		}
		in := follow.ImportFollow{Kind: follow.KindSeries, TmdbID: tmdbID, Title: s.title, Poster: posterOf(s.images),
			Source: SourceSonarr, Import: &follow.ImportState{Baseline: baseline, Want: want}}
		reason := fmt.Sprintf("Включится кнопкой «Перейти на Antresol» · отслеживается %s", i18n.PluralL(i18n.RU, len(want), "серия", "серии", "серий"))
		if err := addFollow(ctx, o, vdb, rep, in, l, reason); err != nil {
			return err
		}
	}
	return nil
}

// sonarrWanted: the regular episodes Sonarr monitors and has no file for.
func sonarrWanted(ctx context.Context, d *arrDB, seriesID int) ([][2]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT COALESCE(SeasonNumber, 0), COALESCE(EpisodeNumber, 0), COALESCE(Monitored, 0), COALESCE(EpisodeFileId, 0) FROM Episodes
		WHERE SeriesId=? AND SeasonNumber>0 ORDER BY SeasonNumber, EpisodeNumber`, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	want := [][2]int{}
	for rows.Next() {
		var season, episode, fileID int
		var monitored bool
		if err := rows.Scan(&season, &episode, &monitored, &fileID); err != nil {
			return nil, err
		}
		if monitored && fileID == 0 {
			want = append(want, [2]int{season, episode})
		}
	}
	return want, rows.Err()
}

// ImportRadarr takes over Radarr's monitored movies without a file as
// inactive «Хочу посмотреть» follows; movies Radarr has are skipped. The
// error is ctx's or Vault's database's.
func ImportRadarr(ctx context.Context, o ArrOptions, vdb *sql.DB, tm TMDBFinder, rep *Report) error {
	d := openArr(o, "Radarr", SourceRadarr, o.RadarrDB, rep)
	if d == nil {
		return ctx.Err()
	}
	defer d.close()
	d.warnRemoveCompleted(ctx, SourceRadarr, rep)

	rows, err := d.QueryContext(ctx, `SELECT COALESCE(MovieMetadata.TmdbId, 0), COALESCE(MovieMetadata.Title, ''), COALESCE(MovieMetadata.Year, 0),
		COALESCE(MovieMetadata.Images, '[]'), COALESCE(Movies.MovieFileId, 0)
		FROM Movies JOIN MovieMetadata ON Movies.MovieMetadataId=MovieMetadata.Id
		WHERE Movies.Monitored=1 ORDER BY Movies.Id`)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.readFailed(SourceRadarr, rep, err)
		return nil
	}
	type movie struct {
		tmdb, year, file int
		title, images    string
	}
	var list []movie
	for rows.Next() {
		var m movie
		if err := rows.Scan(&m.tmdb, &m.title, &m.year, &m.images, &m.file); err != nil {
			rows.Close()
			d.readFailed(SourceRadarr, rep, err)
			return nil
		}
		list = append(list, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		d.readFailed(SourceRadarr, rep, err)
		return nil
	}
	for _, m := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		l := Line{Source: SourceRadarr, Kind: "follow", Name: titleYear(m.title, m.year)}
		switch {
		case m.tmdb <= 0:
			l.Status, l.Reason = StatusFailed, reasonNoMovieID
			rep.Add(l)
			continue
		case m.file > 0:
			l.Status, l.Reason, l.Ref = StatusSkipped, reasonInLibrary, "movie:"+strconv.Itoa(m.tmdb)
			rep.Add(l)
			continue
		}
		in := follow.ImportFollow{Kind: follow.KindMovie, TmdbID: m.tmdb, Title: m.title, Poster: posterOf(m.images), Source: SourceRadarr}
		if err := addFollow(ctx, o, vdb, rep, in, l, reasonSwitchOn); err != nil {
			return err
		}
	}
	return nil
}
