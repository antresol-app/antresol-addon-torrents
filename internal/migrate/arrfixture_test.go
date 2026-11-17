package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Minimal Sonarr v4 / Radarr v6 schemas: the columns the importer reads
// plus a few neighbours, as the real tables have.
// Integer columns older migrations created nullable are nullable here.

type sonarrRow struct {
	ID                   int
	Title                string
	Year, TvdbID, TmdbID int
	Monitored            bool
	Images               string
}

type sonarrEpisode struct {
	SeriesID, Season, Episode int
	Monitored                 bool
	FileID                    int
}

type radarrRow struct {
	Title        string
	Year, TmdbID int
	Monitored    bool
	MovieFileID  int
}

func fixtureDB(t *testing.T, name string, stmts []string, inserts func(*sql.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	inserts(d)
	return path
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

const downloadClientsTable = `CREATE TABLE "DownloadClients" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "Enable" INTEGER NOT NULL,
	"Name" TEXT NOT NULL, "Implementation" TEXT NOT NULL, "Settings" TEXT NOT NULL, "ConfigContract" TEXT NOT NULL,
	"Priority" INTEGER NOT NULL DEFAULT 1, "RemoveCompletedDownloads" INTEGER NOT NULL DEFAULT 1,
	"RemoveFailedDownloads" INTEGER NOT NULL DEFAULT 1, "Tags" TEXT)`

func addDownloadClient(t *testing.T, d *sql.DB, removeCompleted bool) {
	mustExec(t, d, `INSERT INTO DownloadClients (Enable, Name, Implementation, Settings, ConfigContract, RemoveCompletedDownloads)
		VALUES (1, 'qBittorrent', 'QBittorrent', '{}', 'QBittorrentSettings', ?)`, removeCompleted)
	// a disabled client never removes anything
	mustExec(t, d, `INSERT INTO DownloadClients (Enable, Name, Implementation, Settings, ConfigContract, RemoveCompletedDownloads)
		VALUES (0, 'Old', 'Transmission', '{}', 'TransmissionSettings', 1)`)
}

func sonarrDB(t *testing.T, rows []sonarrRow, eps []sonarrEpisode, removeCompleted bool) string {
	t.Helper()
	return fixtureDB(t, "sonarr.db", []string{
		`CREATE TABLE "Series" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "TvdbId" INTEGER NOT NULL, "TvRageId" INTEGER NOT NULL DEFAULT 0,
			"ImdbId" TEXT, "Title" TEXT NOT NULL, "TitleSlug" TEXT, "CleanTitle" TEXT, "Status" INTEGER NOT NULL DEFAULT 0,
			"Images" TEXT, "Path" TEXT, "Monitored" INTEGER NOT NULL, "Year" INTEGER, "Tags" TEXT,
			"MonitorNewItems" INTEGER NOT NULL DEFAULT 0, "TmdbId" INTEGER DEFAULT 0)`,
		`CREATE TABLE "Episodes" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "SeriesId" INTEGER NOT NULL, "SeasonNumber" INTEGER NOT NULL,
			"EpisodeNumber" INTEGER NOT NULL, "Title" TEXT, "Overview" TEXT, "EpisodeFileId" INTEGER DEFAULT 0,
			"AirDate" TEXT, "Monitored" INTEGER NOT NULL DEFAULT 0, "AbsoluteEpisodeNumber" INTEGER, "AirDateUtc" DATETIME)`,
		downloadClientsTable,
	}, func(d *sql.DB) {
		for _, r := range rows {
			images := r.Images
			if images == "" {
				images = "[]"
			}
			mustExec(t, d, `INSERT INTO Series (Id, TvdbId, Title, Images, Path, Monitored, Year, TmdbId) VALUES (?,?,?,?,?,?,?,?)`,
				r.ID, r.TvdbID, r.Title, images, "/srv/media/media/series/"+r.Title, r.Monitored, r.Year, r.TmdbID)
		}
		for _, e := range eps {
			mustExec(t, d, `INSERT INTO Episodes (SeriesId, SeasonNumber, EpisodeNumber, Monitored, EpisodeFileId) VALUES (?,?,?,?,?)`,
				e.SeriesID, e.Season, e.Episode, e.Monitored, e.FileID)
		}
		addDownloadClient(t, d, removeCompleted)
	})
}

func radarrDB(t *testing.T, rows []radarrRow, removeCompleted bool) string {
	t.Helper()
	return fixtureDB(t, "radarr.db", []string{
		`CREATE TABLE "Movies" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "Path" TEXT NOT NULL, "Monitored" INTEGER NOT NULL,
			"QualityProfileId" INTEGER NOT NULL DEFAULT 1, "Added" DATETIME, "Tags" TEXT, "AddOptions" TEXT,
			"MovieFileId" INTEGER DEFAULT 0, "MinimumAvailability" INTEGER NOT NULL DEFAULT 3,
			"MovieMetadataId" INTEGER NOT NULL, "LastSearchTime" DATETIME)`,
		`CREATE TABLE "MovieMetadata" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "TmdbId" INTEGER NOT NULL, "ImdbId" TEXT,
			"Images" TEXT NOT NULL DEFAULT '[]', "Title" TEXT NOT NULL, "OriginalTitle" TEXT, "Year" INTEGER)`,
		downloadClientsTable,
	}, func(d *sql.DB) {
		for i, r := range rows {
			mustExec(t, d, `INSERT INTO MovieMetadata (Id, TmdbId, Title, Year, Images) VALUES (?,?,?,?,?)`,
				100+i, r.TmdbID, r.Title, r.Year, `[{"coverType":"poster","url":"/MediaCover/1/poster.jpg","remoteUrl":"https://image.tmdb.org/t/p/original/`+r.Title+`.jpg"}]`)
			mustExec(t, d, `INSERT INTO Movies (Path, Monitored, MovieFileId, MovieMetadataId) VALUES (?,?,?,?)`,
				"/srv/media/media/movies/"+r.Title, r.Monitored, r.MovieFileID, 100+i)
		}
		addDownloadClient(t, d, removeCompleted)
	})
}
