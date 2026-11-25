// Releases of a title from every source, best first. Each row says what it is
// in plain words — quality, voice-over, size, how many people share it — and
// offers its action: with a `mode` (movies and series) just that mode's
// «Смотреть» or «Загрузить», without one «Смотреть» and «На сервер» (albums
// and books: only «На сервер»; album torrents also «Выбрать треки…»).
// Rejected releases (with the reason) and sources that did not answer are
// listed underneath; only the admin may pick a rejected one (the admin's
// preferences rejected it, so they stay binding for everyone else).
import { useEffect, useState } from 'react'
import { ApiError, formatSize, Icon, ui } from '@antresol/addon-sdk'
import { streamReleases } from '../api'
import type { Release, ReleaseProgress, ReleaseSearch } from '../types'
import { tr, useT, type Key } from '../i18n'
import styles from './ReleaseTable.module.css'

const { Badge, BadgeLabel, Button, Spinner } = ui

export interface ReleaseTableProps {
  kind: string
  /** Movies and series. */
  tmdbId?: number
  /** Music and books: the catalog id, the album or book, its artist or author. */
  extId?: string
  title?: string
  artist?: string
  /** Music: the album's year and its other names. */
  year?: number
  aliases?: string[]
  season?: number
  episode?: number
  /** The release the parent is starting (shows a spinner on its row). */
  busy?: { link: string; mode: 'stream' | 'download' } | null
  /** Movies and series: one action per row, the mode's («Смотреть» or «Загрузить»). Absent: both, as music and books have today. */
  mode?: 'stream' | 'download'
  /** A first row above the list with one action (the mode's): the server's pick, no need to read the list. */
  best?: { label: string; run: () => void; busy?: boolean }
  onPick: (r: Release, mode: 'stream' | 'download') => void
  /** Music: «Выбрать треки…» on each torrent row (a direct file has no file list). */
  onPickTracks?: (r: Release) => void
  /** Music the library has: only «Выбрать треки…» (more songs join the album; a second whole copy would not). */
  tracksOnly?: boolean
  /** The admin: rejected rows can be picked too. */
  admin?: boolean
  /** «Закрепить»: the row's release becomes the title's pinned release (a TMDB title's page). */
  pin?: { onPin: (r: Release) => void; pinnedLink?: string; busyLink?: string }
}

/** ready: the releases so far, `pending` the sources still searched (none: the final set). */
type State = { status: 'loading' } | { status: 'error'; message: string } | { status: 'ready'; data: ReleaseSearch; pending: ReleaseProgress['pending'] }

/** What the server needs to start this release. */
export const releaseLink = (r: Release) => r.link || r.magnet || ''

function quality(r: Release): string {
  if (!r.resolution) return ''
  return r.resolution >= 2160 ? '4K' : `${r.resolution}p`
}

function tags(r: Release): string[] {
  const out: string[] = []
  const q = quality(r)
  if (q) out.push(q)
  if (r.hdr) out.push('HDR')
  if (r.source) out.push(r.source)
  if (r.seasonPack) out.push(tr('comp.release.seasonPack'))
  if (r.direct) out.push(tr('comp.release.direct'))
  for (const a of r.audio ?? []) out.push(a)
  return out
}

const isVideo = (kind: string) => kind === 'movie' || kind === 'series'

/** The language badge of a book or audiobook release: «RU», «EN»; none when unknown. */
export const langBadge = (r: Release) => (r.lang ? r.lang.toUpperCase() : '')

const LANG_NAMES = new Set(['ru', 'en', 'ja', 'uk', 'de', 'fr', 'es', 'it', 'pt', 'pl', 'zh', 'ko'])
const langName = (l: string) => (LANG_NAMES.has(l) ? tr(`comp.release.lang.${l}` as Key) : l)

/** A size of 0 is one the source did not give (a direct file): «—», never «0 Б». */
export const releaseSize = (r: Release) => (r.size > 0 ? formatSize(r.size) : '—')

/** One badge for all subtitle languages: «Субтитры: RU, EN»; «Субтитры» when named without a language. */
function subsBadge(r: Release): string {
  const langs = (r.subs ?? []).filter((l) => l !== 'sub').map((l) => l.toUpperCase())
  if (langs.length > 0) return tr('comp.release.subs', { langs: langs.join(', ') })
  return r.subs?.length ? tr('comp.release.subsAny') : ''
}

/** Video rows: quality, then audio, then subtitles (the music and book rows keep tags()). */
const videoTags = (r: Release) => {
  const subs = subsBadge(r)
  return subs ? [...tags(r), subs] : tags(r)
}

/** A column the person sorted by; null = the server's order (best first). */
export type ReleaseSort = { key: 'quality' | 'size' | 'seeders'; desc: boolean } | null

/**
 * How good a release is by its quality alone: a video's resolution (HDR a
 * notch above), an album's format (lossless over any MP3, then the bitrate;
 * «V0» about 245 kbps); 0 when unknown.
 */
export function qualityRank(r: Release): number {
  if (r.resolution) return r.resolution * 2 + (r.hdr ? 1 : 0)
  const f = (r.audioFormat ?? '').toUpperCase()
  if (!f) return 0
  if (/FLAC|ALAC|WAV|APE|LOSSLESS/.test(f)) return 10_000 + (Number(/24/.test(f)) * 100)
  if (/V0/.test(f)) return 245
  const kbps = /(\d{2,3})/.exec(f)
  return kbps ? Number(kbps[1]) : 1
}

/** `list` in the order of `sort` (a stable sort: equal releases keep the server's order). */
export function sortReleases(list: Release[], sort: ReleaseSort): Release[] {
  if (!sort) return list
  const val = (r: Release) => (sort.key === 'quality' ? qualityRank(r) : sort.key === 'size' ? r.size : r.seeders)
  return [...list].sort((a, b) => (sort.desc ? val(b) - val(a) : val(a) - val(b)))
}

/** The next sort after a click on `key`: descending, then ascending, then the server's order. */
export const nextSort = (cur: ReleaseSort, key: NonNullable<ReleaseSort>['key']): ReleaseSort =>
  cur?.key !== key ? { key, desc: true } : cur.desc ? { key, desc: false } : null

function SortHeader({ sort, onSort }: { sort: ReleaseSort; onSort: (s: ReleaseSort) => void }) {
  const t = useT()
  const cell = (key: NonNullable<ReleaseSort>['key'], label: string, className = '') => {
    const on = sort?.key === key
    // the direction is in the name («Размер, по убыванию»): a pressed state
    // could not tell descending from ascending
    return (
      <button
        type="button"
        className={`${styles.sortCell} ${className} ${on ? styles.sortOn : ''}`}
        aria-label={on ? `${label}, ${t(sort.desc ? 'comp.release.sortDesc' : 'comp.release.sortAsc')}` : undefined}
        title={t('comp.release.sortHint')}
        onClick={() => onSort(nextSort(sort, key))}
      >
        {label}
        {on && <Icon name={sort.desc ? 'chevronDown' : 'chevronUp'} size={12} strokeWidth={2.6} />}
      </button>
    )
  }
  return (
    <div className={`${styles.row} ${styles.sortRow}`} role="group" aria-label={t('comp.release.sortBy')}>
      {cell('quality', t('comp.release.sortQuality'), styles.sortMain)}
      {cell('size', t('comp.release.sortSize'), styles.sortNum)}
      {/* phones hide the seeders column, and so its sort */}
      {cell('seeders', t('comp.release.sortSeeders'), `${styles.sortNum} ${styles.sortSeeds}`)}
      <span className={styles.sortActions} />
    </div>
  )
}

const modeLabel = (mode: ReleaseTableProps['mode']): Key => (mode === 'download' ? 'comp.release.download' : 'common.watch')

interface RowProps {
  r: Release
  busy: ReleaseTableProps['busy']
  onPick: ReleaseTableProps['onPick']
  video: boolean
  mode: ReleaseTableProps['mode']
  onPickTracks?: ReleaseTableProps['onPickTracks']
  tracksOnly?: boolean
  /** Shown without actions (a rejected release, not the admin). */
  readOnly?: boolean
  pin?: ReleaseTableProps['pin']
}

function Row({ r, busy, onPick, video, mode, onPickTracks, tracksOnly, readOnly, pin }: RowProps) {
  const t = useT()
  const link = releaseLink(r)
  const mine = busy?.link === link
  return (
    <li className={`${styles.row} ${r.formatMismatch ? styles.otherFormat : ''}`}>
      <div className={styles.main}>
        <p className={styles.title} title={r.title}>
          {r.title}
        </p>
        <p className={styles.meta}>
          {r.lang && (
            <Badge tone="strong" title={langName(r.lang)}>
              {langBadge(r)}
            </Badge>
          )}
          {r.audioFormat && (
            <Badge tone={r.formatMismatch ? 'warning' : 'strong'} title={r.formatMismatch ? t('comp.release.formatMismatch') : t('comp.release.format')}>
              {r.audioFormat}
            </Badge>
          )}
          {video && r.studios?.length ? (
            <Badge tone="strong" title={t('comp.release.studios', { names: r.studios.join(', ') })}>
              {r.studios[0]}
            </Badge>
          ) : null}
          {(video ? videoTags(r) : tags(r)).map((tag) => (
            <Badge key={tag}>
              <BadgeLabel text={tag} />
            </Badge>
          ))}
          <span>{r.sourceName}</span>
        </p>
        {r.excluded && r.reasons.length > 0 && <p className={styles.reasons}>{r.reasons.join(' · ')}</p>}
        {!r.excluded && r.formatMismatch && <p className={styles.reasons}>{t('comp.release.mismatchNote')}</p>}
      </div>
      <span className={styles.size} title={r.size > 0 ? undefined : t('comp.release.sizeUnknown')}>
        {releaseSize(r)}
      </span>
      <span className={styles.seeds} title={t('comp.release.seeders')}>
        <Icon name="arrowUp" size={14} strokeWidth={2.4} />
        {r.seeders}
      </span>
      {/* rejected rows too, for the admin: they may knowingly want e.g. a big pack */}
      <span className={styles.actions}>
        {!readOnly && pin && link && (
          <Button
            size="small"
            variant="secondary"
            disabled={!!busy || !!pin.busyLink || pin.pinnedLink === link}
            loading={pin.busyLink === link}
            aria-label={pin.pinnedLink === link ? undefined : t('pin.pinOf', { title: r.title })}
            onClick={() => pin.onPin(r)}
          >
            {pin.pinnedLink === link ? t('pin.pinned') : t('pin.pin')}
          </Button>
        )}
        {readOnly ? null : video && mode ? (
          <Button size="small" disabled={!!busy || !link} loading={mine && busy?.mode === mode} onClick={() => onPick(r, mode)}>
            {t(modeLabel(mode))}
          </Button>
        ) : (
          <>
            {video && (
              <Button size="small" variant="secondary" disabled={!!busy || !link} loading={mine && busy?.mode === 'stream'} onClick={() => onPick(r, 'stream')}>
                {t('common.watch')}
              </Button>
            )}
            {onPickTracks && !r.direct && (
              <Button size="small" variant="secondary" disabled={!!busy || !link} aria-label={t('comp.release.pickTracksOf', { title: r.title })} onClick={() => onPickTracks(r)}>
                {t('comp.release.pickTracks')}
              </Button>
            )}
            {!(tracksOnly && onPickTracks) && (
              <Button size="small" disabled={!!busy || !link} loading={mine && busy?.mode === 'download'} onClick={() => onPick(r, 'download')}>
                {t('comp.action.toServer')}
              </Button>
            )}
          </>
        )}
      </span>
    </li>
  )
}

export function ReleaseTable({ tmdbId, kind, extId, title, artist, year, aliases, season, episode, busy, mode, best, onPick, onPickTracks, tracksOnly, admin = false, pin }: ReleaseTableProps) {
  const t = useT()
  const video = isVideo(kind)
  const [state, setState] = useState<State>({ status: 'loading' })
  const [showRejected, setShowRejected] = useState(false)
  const [sort, setSort] = useState<ReleaseSort>(null)
  const [attempt, setAttempt] = useState(0)
  const aliasKey = (aliases ?? []).join('\n') // a new array per render must not search again

  useEffect(() => {
    const ac = new AbortController()
    setState({ status: 'loading' })
    const q = video ? { kind, tmdbId, season, episode } : { kind, extId, title, artist, year: year || undefined, alias: aliasKey ? aliasKey.split('\n') : undefined }
    // the list grows as each source answers (a slow one may hang until its timeout)
    streamReleases(q, (p) => setState({ status: 'ready', data: p, pending: p.pending }), ac.signal).then(
      (data) => setState({ status: 'ready', data, pending: [] }),
      (e: unknown) => {
        if (!ac.signal.aborted) setState({ status: 'error', message: e instanceof ApiError ? e.message : tr('comp.release.searchFailed') })
      },
    )
    return () => ac.abort()
  }, [video, tmdbId, kind, extId, title, artist, year, aliasKey, season, episode, attempt])

  // the server's own pick never waited for the search: shown in every state
  const bestRow = best && (
    <div className={`${styles.row} ${styles.best}`} data-best>
      <p className={styles.title}>{best.label}</p>
      <span className={styles.actions}>
        <Button size="small" disabled={!!busy || best.busy} loading={best.busy} onClick={best.run}>
          {t(modeLabel(mode))}
        </Button>
      </span>
    </div>
  )
  // no source has answered with a release yet: still the search's spinner
  const nothingYet = state.status === 'ready' && state.pending.length > 0 && state.data.releases.length + state.data.rejected.length === 0
  if (state.status === 'loading' || nothingYet) {
    return (
      <div className={styles.table}>
        {bestRow}
        <div className={styles.state} role="status">
          <Spinner track className={styles.spinner} />
          {t('comp.release.searching')}
        </div>
      </div>
    )
  }
  if (state.status === 'error') {
    return (
      <div className={styles.table}>
        {bestRow}
        <div className={styles.state} role="alert">
          <p>{state.message}</p>
          <Button size="small" variant="secondary" onClick={() => setAttempt((n) => n + 1)}>
            {t('common.retry')}
          </Button>
        </div>
      </div>
    )
  }
  const { releases, rejected, failed } = state.data
  const { pending } = state
  return (
    <div className={styles.table}>
      {/* «the other sources were checked»: only once all of them were */}
      {failed.length > 0 && pending.length === 0 && (
        <p className={styles.failed} role="status">
          <Icon name="warning" size={16} />
          {t('comp.release.failedSources', { names: failed.map((f) => f.name).join(', ') })}
        </p>
      )}
      {bestRow}
      {/* one announcement while the sources answer, one when all have: the
          visible line below changes with each answer and stays silent */}
      <p className="visually-hidden" role="status">
        {pending.length > 0 ? t('comp.release.searching') : t('comp.release.found', { count: releases.length })}
      </p>
      {pending.length > 0 && (
        <p className={styles.pending} aria-live="off">
          <Spinner track className={styles.miniSpinner} />
          {t('comp.release.pending', { names: [...new Set(pending.map((p) => p.name))].join(', ') })}
        </p>
      )}
      {/* the header and both lists share one grid: their columns line up */}
      <div className={styles.grid}>
        {releases.length > 1 && <SortHeader sort={sort} onSort={setSort} />}
        {releases.length > 0 ? (
          <ul className={styles.list} aria-label={t('comp.release.list')}>
            {sortReleases(releases, sort).map((r) => (
              <Row key={`${r.sourceId}-${releaseLink(r)}-${r.title}`} r={r} busy={busy} onPick={onPick} video={video} mode={mode} onPickTracks={onPickTracks} tracksOnly={tracksOnly} pin={pin} />
            ))}
          </ul>
        ) : (
          pending.length === 0 && <p className={styles.state}>{t('comp.release.none')}</p>
        )}
        {rejected.length > 0 && (
          <>
            <button type="button" className={styles.more} aria-expanded={showRejected} onClick={() => setShowRejected((v) => !v)}>
              <Icon name={showRejected ? 'chevronDown' : 'chevronRight'} size={14} strokeWidth={2.4} />
              {t('comp.release.rejectedCount', { count: rejected.length })}
            </button>
            {showRejected && (
              <ul className={`${styles.list} ${styles.rejected}`} aria-label={t('comp.release.rejected')}>
                {sortReleases(rejected, sort).map((r) => (
                  <Row key={`${r.sourceId}-${releaseLink(r)}-${r.title}`} r={r} busy={busy} onPick={onPick} video={video} mode={mode} onPickTracks={onPickTracks} tracksOnly={tracksOnly} readOnly={!admin} pin={pin} />
                ))}
              </ul>
            )}
          </>
        )}
      </div>
    </div>
  )
}
