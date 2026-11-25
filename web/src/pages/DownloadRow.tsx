// One download of «Загрузки», as a row of a System Settings list:
//   poster │ title                                   │ [action] [⋯]
//          │ status · percent · size · speed · peers  │
//          │ ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ progress  │
//          │ a note, if any (stalled, switched, error)│
// Every row has the same poster, the same paddings and the same trailing
// column — one action slot (pause, resume, «На сервер»; empty when there
// is nothing to do) and the «⋯» menu (pause seeding, delete) — so the
// buttons line up from row to row. A download has a progress bar; a
// seeding row is quiet: no bar, a grey status with a green ↑.
import { memo, useEffect, useState } from 'react'
import { useNavigate } from 'react-router'
import { ApiError, Icon, type IconName, mediaKindIcon, formatPercent, formatRate, formatSize, useIsCompact, ui, core } from '@antresol/addon-sdk'
import { deleteTorrent, dismissTorrentNote, forceOrganizeTorrent, organizeTorrent, pauseTorrent, promoteTorrent, resumeTorrent } from '../api'
import { type Torrent } from '../types'
import { StalledNote } from '../StalledNote'
import { tr, useT, type Key } from '../i18n'
import { torrentStatusLabel } from '../format'
import styles from './DownloadRow.module.css'

const { Sheet, Badge, Button, Checkbox, IconButton, SwipeRow, Menu, MenuItem, MenuSeparator, ProgressBar } = ui

const posterCache = new Map<number, string>()

/** The cover: the one the server kept when the download was added, else the library item's. */
function usePoster(t: Torrent): string | undefined {
  const [src, setSrc] = useState(() => (t.itemId != null ? posterCache.get(t.itemId) : undefined))
  useEffect(() => {
    if (t.poster || src || t.itemId == null) return
    const ac = new AbortController()
    core.getItem(t.itemId, ac.signal).then(
      (it) => {
        posterCache.set(t.itemId!, it.poster)
        setSrc(it.poster)
      },
      () => {},
    )
    return () => ac.abort()
  }, [t.itemId, t.poster, src])
  return core.imageUrl(t.poster || src, 160)
}

/** A direct download's status (queued = waiting for a free slot). */
export function directStatusLabel(status: Torrent['status']): string {
  switch (status) {
    case 'queued':
      return tr('downloads.direct.queued')
    case 'downloading':
      return tr('common.download.downloading')
    case 'done':
      return tr('downloads.direct.done')
    case 'error':
      return tr('common.error')
    default:
      return status
  }
}

/** What a download becomes in the library, for «… останется в медиатеке». */
function kindNoun(kind: Torrent['kind']): Key {
  switch (kind) {
    case 'series':
      return 'downloads.noun.series'
    case 'album':
    case 'music':
      return 'downloads.noun.album'
    case 'book':
      return 'downloads.noun.book'
    case 'audiobook':
      return 'downloads.noun.audiobook'
    case 'comic':
      return 'downloads.noun.comic'
    case 'voiceover': // «Найти озвучку…»: tracks beside the library's videos
      return 'downloads.noun.voiceover'
    default:
      return 'downloads.noun.movie'
  }
}

/**
 * The organize error of a music torrent whose CUE or tags name another album (the admin may place it anyway).
 * Server text: matched in both interface languages.
 */
const OTHER_ALBUM = ['Скачан другой альбом', 'A different album was downloaded']

/**
 * The error the engine clears on resume: a write error (a full or lost
 * disk), «ошибка записи: …» — writeErrorPrefix of internal/torrent
 * (holds.go). Any other error stays after a resume, so «Повторить» would
 * do nothing.
 */
const WRITE_ERROR = 'ошибка записи'
export const retryable = (t: Torrent) => !!t.error?.startsWith(WRITE_ERROR)

/** The look of a row: what its status word, bar and action are. */
export type RowState = 'downloading' | 'paused' | 'waiting' | 'error' | 'seeding' | 'done'

export function rowState(t: Torrent): RowState {
  if (t.status === 'error') return 'error'
  if (t.parked && t.status === 'seeding') return 'done'
  if (t.status === 'seeding' || (t.direct && t.status === 'done')) return t.direct ? 'done' : 'seeding'
  if (t.status === 'waiting_space') return 'waiting'
  if (t.status === 'paused' || t.dataMissing) return 'paused'
  return 'downloading'
}

/**
 * «Выбрать другую раздачу…» of a stalled download: the release sheet on the
 * title's page (?replace=1), for a movie or series download that the library
 * has no page of yet; undefined otherwise. A series names its season, or
 * season=all for a whole-series pack (season 0), so the sheet lists packs
 * and the replace cancels the pack, not season 1.
 */
export function replacePath(t: Torrent): string | undefined {
  if (t.mode !== 'download' || !t.tmdbId || t.itemId != null || (t.kind !== 'movie' && t.kind !== 'series')) return undefined
  return `/title/${t.kind}/${t.tmdbId}?replace=1${t.kind === 'series' ? `&season=${t.season || 'all'}` : ''}`
}

/**
 * Two torrents that draw the same row: the same fields, all equal.
 * torrent.progress re-sends the whole list every 2 s with new objects; most
 * rows did not change.
 */
export function sameTorrent(a: Torrent, b: Torrent): boolean {
  if (a === b) return true
  const keys = Object.keys(a) as (keyof Torrent)[]
  return keys.length === Object.keys(b).length && keys.every((k) => k in b && a[k] === b[k])
}

type RowProps = { t: Torrent; onChanged: () => void }

/** A row renders again only when its torrent changed (or the list's onChanged did). */
export const DownloadRow = memo(DownloadRowView, (prev: RowProps, next: RowProps) => prev.onChanged === next.onChanged && sameTorrent(prev.t, next.t))

function DownloadRowView({ t, onChanged }: RowProps) {
  const tl = useT()
  const navigate = useNavigate()
  const compact = useIsCompact()
  const poster = usePoster(t)
  const [busy, setBusy] = useState('')
  const [problem, setProblem] = useState('')
  const [note, setNote] = useState('')
  const [confirm, setConfirm] = useState(false)
  const [withFiles, setWithFiles] = useState(false)
  const name = t.title || t.name || tl('common.untitled')
  // some songs of an album («Выбрать треки…»): «Группа крови — 2 трека из 12»
  const tracksOf =
    t.kind === 'music' && !!t.selectedTracks && !!t.totalTracks && t.selectedTracks < t.totalTracks
      ? tl('downloads.tracksOf', { album: name, count: t.selectedTracks, total: t.totalTracks })
      : ''
  const state = rowState(t)
  const progress = t.size > 0 ? t.done / t.size : 0
  const stream = t.mode === 'stream'
  // A direct download of an OPDS source: no peers, no pause, no promote.
  const direct = !!t.direct
  const finished = t.size > 0 && t.done >= t.size
  const running = !direct && (t.status === 'downloading' || t.status === 'queued')
  const stalled = running ? t.stalled : undefined

  const act = async (what: string, f: () => Promise<unknown>) => {
    setBusy(what)
    setProblem('')
    setNote('')
    try {
      await f()
      onChanged()
    } catch (e) {
      setProblem(e instanceof ApiError ? e.message : tl('downloads.failedRetry'))
    } finally {
      setBusy('')
    }
  }

  const keep = () =>
    act('keep', async () => {
      const r = await promoteTorrent(t.infoHash)
      setNote(r.status === 'waiting_space' ? tl('downloads.keepWaiting') : tl('downloads.kept'))
    })

  // the status line after the word: how far, how big, how fast, who added it
  const meta: string[] = []
  const down = t.downRate > 0 ? `↓ ${formatRate(t.downRate)}` : ''
  if (state === 'seeding') {
    meta.push(tl('downloads.uploaded', { size: formatSize(t.uploaded) }))
    if (t.upRate > 0) meta.push(`↑ ${formatRate(t.upRate)}`)
  } else if (direct && t.size <= 0) meta.push(formatSize(t.done))
  else {
    // a phone keeps the line short: the bar shows how far, the peers are detail
    if (state !== 'done' && state !== 'error' && t.size > 0 && !compact) meta.push(formatPercent(Math.floor(progress * 100)))
    meta.push(tl('downloads.doneOf', { done: formatSize(t.done), size: t.size > 0 ? formatSize(t.size) : '…' }))
    if (down) meta.push(down)
    if (running && t.peers > 0 && !compact) meta.push(tl('downloads.peers', { count: t.peers }))
  }
  if (!tracksOf && !!t.selectedFiles && !!t.totalFiles && t.selectedFiles < t.totalFiles) meta.push(tl('downloads.filesSelected', { n: t.selectedFiles, total: t.totalFiles }))
  // whose it is: the admin sees the whole household's downloads
  // (a phone leaves it out: the line must stay short beside the poster)
  if (t.addedByName && !compact) meta.push(tl('downloads.addedBy', { name: t.addedByName }))

  // What can be done to the row, in order. The first one in the slot list
  // is the row's action in the trailing slot; «⋯» lists them all, then
  // «Удалить…» — never a menu of one item.
  type Action = { key: string; icon: IconName; label: string; tooltip?: string; slot?: boolean; run: () => void }
  const actions: Action[] = []
  const resumable = !direct && (state === 'paused' || state === 'waiting' || !!t.dataMissing)
  if (stream && state !== 'seeding')
    actions.push({ key: 'keep', icon: 'download', label: tl('downloads.toServer'), tooltip: tl('downloads.keepHint'), slot: running, run: () => void keep() })
  if (running) actions.push({ key: 'pause', icon: 'pause', label: tl('common.pause'), slot: true, run: () => void act('pause', () => pauseTorrent(t.infoHash)) })
  if (resumable)
    actions.push({
      key: 'resume',
      // waiting for room: a download, not a Play
      icon: state === 'waiting' ? 'arrowDown' : 'play',
      label: state === 'waiting' ? tl('downloads.downloadAnyway') : tl('common.resume'),
      slot: true,
      run: () => void act('resume', () => resumeTorrent(t.infoHash)),
    })
  // a failed download the engine can retry (a write error) starts again…
  else if (!direct && state === 'error' && retryable(t))
    actions.push({ key: 'retry', icon: 'restart', label: tl('common.retry'), slot: true, run: () => void act('retry', () => resumeTorrent(t.infoHash)) })
  // …else the title's release sheet offers another release (the row's action)
  const other = state === 'error' ? replacePath(t) : undefined
  if (other) actions.push({ key: 'other', icon: 'search', label: tl('item.download.other'), slot: true, run: () => navigate(other) })
  if (state === 'seeding') actions.push({ key: 'pause', icon: 'pause', label: tl('downloads.pauseSeeding'), run: () => void act('pause', () => pauseTorrent(t.infoHash)) })
  const slot = actions.find((a) => a.slot)
  const primary = slot ? (
    <IconButton size="sm" icon={slot.icon} label={slot.label} tooltip={slot.tooltip} loading={busy === slot.key} disabled={!!busy} onClick={slot.run} />
  ) : null

  const statusWord =
    t.parked && t.status === 'seeding'
      ? tl('downloads.parked')
      : direct
        ? directStatusLabel(t.status)
        : t.status === 'seeding'
          ? tl('common.download.seeding')
          : torrentStatusLabel(t.status, t.mode)
  const bar = state === 'downloading' || state === 'paused' || state === 'waiting'
  const organizeFix = !!t.organizeError && finished

  return (
    // a finger swipes the row for its action and «Удалить» (also in «⋯»)
    <SwipeRow
      className={styles.item}
      contentClassName={styles.row}
      data-testid="torrent-card"
      data-state={state}
      actions={[
        ...(slot ? [{ key: slot.key, label: slot.label, icon: slot.icon, tone: 'accent' as const, disabled: !!busy, onSelect: slot.run }] : []),
        {
          key: 'delete',
          label: tl('common.delete'),
          icon: 'trash' as const,
          tone: 'destructive' as const,
          disabled: !!busy,
          onSelect: () => (setWithFiles(false), setConfirm(true)),
        },
      ]}
    >
      <div className={styles.poster}>{poster ? <img src={poster} alt="" loading="lazy" /> : <Icon name={mediaKindIcon(t.kind)} size={20} />}</div>
      <div className={styles.main}>
        <p className={styles.name} title={t.name}>
          {tracksOf || name}
        </p>
        <p className={styles.status}>
          <span className={styles.word}>
            {state === 'seeding' && <Icon name="arrowUp" size={12} strokeWidth={2.6} className={styles.seedIcon} />}
            {statusWord}
          </span>
          {stream && state !== 'seeding' && <Badge round>{tl('downloads.badge.stream')}</Badge>}
          {direct && <Badge round>{tl('downloads.badge.direct')}</Badge>}
          {meta.map((x) => (
            <span key={x} className={styles.meta}>
              {x}
            </span>
          ))}
        </p>
        {bar && <ProgressBar className={styles.bar} value={progress} tone={state === 'downloading' ? 'accent' : state === 'waiting' ? 'warning' : 'paused'} label={tl('comp.download.progress')} />}
        {t.autoReplace === 'switched' && (
          <p className={styles.note} role="status" data-testid="auto-switched">
            {tl('downloads.autoSwitched', { name: t.name || name })}{' '}
            <Button variant="link" disabled={!!busy} onClick={() => void act('dismiss', () => dismissTorrentNote(t.infoHash))}>
              {tl('downloads.autoSwitchedHide')}
            </Button>
          </p>
        )}
        <StalledNote stalled={stalled} hint={running ? t : undefined} noneAlive={running && t.autoReplace === 'none_alive'} replaceTo={replacePath(t)} />
        {/* the torrent failed: an error; a problem putting it into the library: a warning */}
        {(t.error || t.organizeError) && (
          <p className={styles.problem} data-tone={t.error ? 'error' : 'warning'}>
            {t.error || t.organizeError}
            {organizeFix && (
              <>
                {' '}
                <Button variant="link" disabled={!!busy} onClick={() => void act('organize', () => organizeTorrent(t.infoHash))}>
                  {tl('downloads.addToLibrary')}
                </Button>
              </>
            )}
            {organizeFix && OTHER_ALBUM.some((p) => t.organizeError?.startsWith(p)) && (
              <>
                {' · '}
                <Button variant="link" disabled={!!busy} onClick={() => void act('force', () => forceOrganizeTorrent(t.infoHash))}>
                  {tl('downloads.organizeAnyway')}
                </Button>
              </>
            )}
          </p>
        )}
        {t.note && <p className={styles.note}>{t.note}</p>}
        {problem && (
          <p className={styles.problem} data-tone="error" role="alert">
            {problem}
          </p>
        )}
        {note && (
          <p className={styles.done} role="status">
            {note}
          </p>
        )}
      </div>
      <div className={styles.trailing}>
        {/* a phone keeps one button, «⋯»: the row's action is in it and a swipe away */}
        {!compact && (primary ?? <span className={styles.slot} aria-hidden />)}
        {/* nothing to do but delete: the trash in the «⋯» column, never a menu of one item */}
        {actions.length === 0 ? (
          <IconButton
            size="sm"
            icon="trash"
            tone="destructive"
            label={tl('downloads.deleteNamed', { title: name })}
            disabled={!!busy}
            onClick={() => (setWithFiles(false), setConfirm(true))}
          />
        ) : (
        <Menu
          label={tl('downloads.actions', { title: name })}
          placement="bottom-end"
          // a phone has no slot button: «⋯» carries the spinner while an action runs
          trigger={(p) => <IconButton {...p} size="sm" icon="ellipsis" label={tl('downloads.actions', { title: name })} disabled={!!busy} loading={compact && !!busy} />}
        >
          {(close) => (
            <>
              {actions.map((a) => (
                <MenuItem key={a.key + a.label} icon={<Icon name={a.icon} size={16} />} label={a.label} onSelect={() => (close(a.key !== 'other'), a.run())} />
              ))}
              {actions.length > 0 && <MenuSeparator />}
              <MenuItem
                destructive
                icon={<Icon name="trash" size={16} />}
                label={tl('downloads.deleteMenu')}
                onSelect={() => {
                  close(false)
                  setWithFiles(false)
                  setConfirm(true)
                }}
              />
            </>
          )}
        </Menu>
        )}
      </div>
      {confirm && (
        <Sheet
          title={tl('downloads.deleteConfirm', { title: name })}
          message={
            direct
              ? t.status === 'done'
                ? tl('downloads.deleteDirectDone', { noun: tl(kindNoun(t.kind)) })
                : tl('downloads.deleteStops')
              : t.parked && t.status === 'seeding'
                ? tl('downloads.deleteDirectDone', { noun: tl(kindNoun(t.kind)) })
              : t.status === 'seeding'
                ? tl('downloads.deleteSeeding', { noun: tl(kindNoun(t.kind)) })
                : tl('downloads.deleteKeepPartial')
          }
          icon={<Icon name="trash" size={34} strokeWidth={1.6} />}
          busy={busy === 'delete'}
          onCancel={() => setConfirm(false)}
          actions={[
            { label: tl('common.cancel'), kind: 'cancel', run: () => setConfirm(false) },
            {
              label: tl('common.delete'),
              kind: 'destructive',
              run: () => void act('delete', () => deleteTorrent(t.infoHash, withFiles)).then(() => setConfirm(false)),
            },
          ]}
        >
          {!direct && <Checkbox className={styles.check} tone="destructive" label={tl('downloads.deleteFiles')} checked={withFiles} onChange={setWithFiles} />}
        </Sheet>
      )}
    </SwipeRow>
  )
}
