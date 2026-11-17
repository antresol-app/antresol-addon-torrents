package coreadapt

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol/addonkit"
)

// The core's limits of a letter (POST /notify, per language): longer
// texts are cut, not refused.
const (
	maxSubject = 200
	maxBody    = 2000
)

// cut is s at most n characters long.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// downloadsPage is where a letter about a download opens when the library
// has nothing to show for it.
const downloadsPage = "/addons/torrents/downloads"

// Finished is a download that finished or failed: acquire.Finished
// field for field (acquire imports this package, so boot converts it:
// coreadapt.Finished(f)).
type Finished struct {
	Key    string // the infohash, or the direct download's id
	Kind   string
	Title  string
	Year   int
	Failed bool
	Error  string
}

// Notifier sends acquire's finished downloads to the admins as letters
// through the core (POST /notify, topic downloads). T is the
// catalog's TL; ItemPath is the core's page of the title a download (by
// its key) brought ("" when the library has none).
type Notifier struct {
	C        *addonkit.Client
	T        func(lang, key string, args ...any) string
	ItemPath func(ctx context.Context, key string) string
}

// Finished queues the letter of f: «Загрузка завершилась: <title (year)>»
// linking to the title's page (else the downloads page), or «Загрузка не
// удалась: …» with the localized error as its body, linking to the
// downloads page. Both languages are filled from the catalog. One key
// per download and outcome, so a retried callback makes no second line.
func (n Notifier) Finished(ctx context.Context, f Finished) error {
	if f.Key == "" {
		return nil
	}
	title := f.Title
	if f.Year > 0 {
		title += " (" + strconv.Itoa(f.Year) + ")"
	}
	both := func(key string, args ...any) addonkit.Text {
		return addonkit.Text{RU: cut(n.T(i18n.RU, key, args...), maxSubject), EN: cut(n.T(i18n.EN, key, args...), maxSubject)}
	}
	req := addonkit.NotifyRequest{Topic: "downloads", Audience: "admins", Key: "download:" + f.Key + ":done",
		Subject: both("Загрузка завершилась: %s", title), Link: downloadsPage}
	if f.Failed {
		req.Key, req.Subject = "download:"+f.Key+":failed", both("Загрузка не удалась: %s", title)
		if f.Error != "" {
			req.Body = &addonkit.Text{RU: cut(i18n.LocalizeL(i18n.RU, f.Error), maxBody), EN: cut(i18n.LocalizeL(i18n.EN, f.Error), maxBody)}
		}
	} else if n.ItemPath != nil {
		if p := n.ItemPath(ctx, f.Key); p != "" {
			req.Link = p
		}
	}
	_, err := n.C.Notify(ctx, req)
	return err
}

// AcquisitionItemPath is Notifier.ItemPath over the addon's acquisitions:
// the core's /item/<id> of the movie or series a download brought (the
// core's notify.itemPath), "" for other kinds, a direct download or a
// title the library does not have.
func AcquisitionItemPath(db *sql.DB, c *addonkit.Client) func(ctx context.Context, key string) string {
	return func(ctx context.Context, key string) string {
		var kind string
		var tmdb int64
		err := db.QueryRowContext(ctx, `SELECT kind, tmdb_id FROM acquisitions WHERE infohash=lower(?)`, key).Scan(&kind, &tmdb)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("coreadapt: letter link: acquisition", "key", key, "err", err)
		}
		if err != nil || tmdb <= 0 || (kind != "movie" && kind != "series") {
			return ""
		}
		it, ok, err := c.ItemByTMDB(ctx, kind, tmdb)
		if err != nil {
			slog.Warn("coreadapt: letter link: library", "key", key, "err", err)
		}
		if err != nil || !ok || it.ID <= 0 {
			return ""
		}
		return "/item/" + strconv.FormatInt(it.ID, 10)
	}
}
