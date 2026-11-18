package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// ErrQBUnreachable: a rollback could not reach qBittorrent, so it did
// nothing; the person connects it (again) and repeats.
var ErrQBUnreachable = errors.New("migrate: qBittorrent cannot be reached")

// reasonKeptPaused is a rollback line's reason while torrents must stay
// paused (QBOptions.KeepPaused says so); reasonNoRule is the reason when the
// caller set no KeepPaused: they go back paused all the same (fail closed),
// but no rule is claimed.
const (
	reasonKeptPaused = "Раздачи возвращены на паузе: включён запрет раздачи"
	reasonNoRule     = "Раздачи возвращены на паузе: запустите их в qBittorrent вручную, если нужно"
)

// RollbackQB gives back to qBittorrent every torrent that the report from
// moved (lines from qBittorrent with status imported): Vault removes its
// copy (the data stays; a torrent already gone is fine), then qBittorrent
// starts it again — Vault first, so the two never seed at once. Only
// torrents that were running in qBittorrent before the move (Line.Prev,
// or the state file for older lines) are started; paused ones stay paused.
// While o.KeepPaused says so (asked per line, right before the start), none
// is started: every one goes back paused (reasonKeptPaused). With no
// KeepPaused at all the same happens (fail closed), with reasonNoRule.
// A torrent Vault no longer has is started only while all its files are on
// disk (qBittorrent would download missing ones again). Nothing is removed
// when qBittorrent cannot be reached. Outcomes go to rep, with a Checkpoint
// after each line. The error is ctx's, or ErrQBUnreachable when qBittorrent
// could not be reached: nothing was done, so the rollback is not finished
// and may be asked again.
func RollbackQB(ctx context.Context, o QBOptions, eng TorrentEngine, from *Report, rep *Report) error {
	log := slog.Default().With("component", "migrate")
	add := func(l Line) {
		l.Source, l.Kind = SourceQBittorrent, "torrent"
		rep.Add(l)
		if o.Checkpoint != nil {
			o.Checkpoint()
		}
	}
	todo := movedTorrents(from)
	if len(todo) == 0 {
		return nil
	}
	qb := newQBClient(o)
	bk := newQBBackup(o, qb)
	if err := connectQB(ctx, o, qb); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		shown := redactURL(o.WebURL)
		log.Warn("qbittorrent api", "url", shown, "err", err)
		reason := "Нет связи с qBittorrent — ничего не возвращено; подключите его на странице переноса и повторите"
		if shown != "" {
			reason = fmt.Sprintf("Нет связи с qBittorrent (%s) — ничего не возвращено; проверьте подключение и повторите", shown)
		}
		add(Line{Name: "qBittorrent", Status: StatusFailed, Reason: reason})
		return ErrQBUnreachable
	}
	for _, src := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Fail closed: a line without a known qBittorrent state (and no
		// valid state file) is removed from Vault but never started.
		prev := src.Prev
		if prev != PrevRunning && prev != PrevStopped {
			prev = ""
			if st, ok, err := loadState(o.StateDir, src.Ref); err != nil {
				log.Warn("rollback: move state", "infohash", src.Ref, "err", err)
			} else if ok {
				prev = st.Prev
			}
		}
		l := Line{Name: src.Name, Ref: src.Ref, Size: src.Size, Prev: prev}
		if o.DryRun {
			l.Status, l.Reason = StatusPlanned, "Будет возвращена в qBittorrent"
			add(l)
			continue
		}
		err := eng.RemoveIfMode(ctx, src.Ref, torrent.ModeSeed, false)
		gone := errors.Is(err, torrent.ErrNotFound)
		switch {
		case errors.Is(err, torrent.ErrModeChanged):
			l.Status, l.Reason = StatusFailed, "В Antresol эта раздача уже не в режиме раздачи — не трогаю"
			add(l)
			continue
		case err != nil && !gone:
			log.Warn("rollback: remove from vault", "infohash", src.Ref, "err", err)
			l.Status, l.Reason = StatusFailed, fmt.Sprintf("Не удалось убрать раздачу из Antresol: %s", err)
			add(l)
			continue
		}
		removeLinkTree(o.LinkDir, src.Ref, log)
		// asked per line, right before a start: no-seeding turned on during
		// a rollback keeps the rest paused
		if o.KeepPaused == nil || o.KeepPaused(ctx) {
			// Antresol's copy is gone; qBittorrent keeps it as it is now
			// (stopped by the move), never started
			removeState(o.StateDir, src.Ref)
			l.Status, l.Reason = StatusImported, reasonKeptPaused
			if o.KeepPaused == nil {
				l.Reason = reasonNoRule
			}
			add(l)
			continue
		}
		if prev == "" {
			l.Status, l.Reason = StatusFailed, "Неизвестно, раздавалась ли она в qBittorrent — запустите там вручную, если нужно"
			add(l)
			continue
		}
		if prev != PrevRunning {
			removeState(o.StateDir, src.Ref)
			l.Status, l.Reason = StatusImported, "Возвращена в qBittorrent (на паузе, как была)"
			add(l)
			continue
		}
		if gone && !filesOnDisk(ctx, o, bk, src.Ref) {
			// Removed in Vault, maybe with its data: starting it would make
			// qBittorrent download it again.
			l.Status, l.Reason = StatusFailed, "Файлов раздачи нет на диске — в qBittorrent не запускаю"
			add(l)
			continue
		}
		if err := qb.start(context.WithoutCancel(ctx), src.Ref); err != nil {
			log.Warn("rollback: start in qbittorrent", "infohash", src.Ref, "err", err)
			l.Status, l.Reason = StatusFailed, fmt.Sprintf("Antresol больше не раздаёт её, но запустить в qBittorrent не удалось: %s — запустите её в qBittorrent вручную", err)
			add(l)
			continue
		}
		removeState(o.StateDir, src.Ref)
		l.Status, l.Reason = StatusImported, "Возвращена в qBittorrent"
		add(l)
	}
	return nil
}

// filesOnDisk reports whether every non-empty file of the torrent is where
// qBittorrent keeps it (from BT_backup or its Web API). Anything
// unreadable counts as no.
func filesOnDisk(ctx context.Context, o QBOptions, bk qbBackup, ih string) bool {
	meta, reason, _ := bk.torrent(ctx, ih)
	if reason != "" {
		return false
	}
	mi, err := metainfo.Load(bytes.NewReader(meta))
	if err != nil {
		return false
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return false
	}
	fr, reason, _ := bk.resume(ctx, ih)
	if reason != "" {
		return false
	}
	files := info.UpvertedFiles()
	for i, f := range layoutOf(o.Paths, &info, fr).files {
		if files[i].Length == 0 {
			continue
		}
		if st, err := os.Stat(f.Target); err != nil || st.Size() != files[i].Length {
			return false
		}
	}
	return true
}
