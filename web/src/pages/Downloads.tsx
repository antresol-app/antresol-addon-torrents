// «Загрузки»: every torrent as a row of a list (DownloadRow) — poster,
// name, status, progress, the row's action and its «⋯» menu. Live through
// torrent.progress. Downloads first, then the seeding ones. A segmented
// switcher picks the section (films, series, music, books) and a menu sorts
// and filters; both live in the URL (?kind, ?sort, ?watch, ?status).
import { ApiError, useEvents, Icon, MEDIA_SEGMENTS, mediaSegmentIcon, mediaSegmentOf, useMediaSegments, useUrlView, ViewMenu, ViewToolbar, type MediaSegment, type ViewSpec, locale, ui, core } from '@antresol/addon-sdk'
import { getTorrents } from '../api'
import { type Torrent } from '../types'
import { useT, type Key } from '../i18n'
import { setTitle } from '../title'
import { useCallback, useEffect, useRef, useState } from 'react'
import { DownloadRow } from './DownloadRow'
import m from './Manage.module.css'

const { Button, SegmentedControl, GroupedSection } = ui

export { directStatusLabel, replacePath } from './DownloadRow'

type State = { status: 'loading' } | { status: 'error'; message: string } | { status: 'ready'; list: Torrent[] }

/** How often the «disk offline» banner is re-checked. */
const DISK_CHECK_MS = 15_000

// --- sections, sort and filters ---

export type DownloadSegment = MediaSegment
export type DownloadSort = 'added' | 'name' | 'size' | 'state'
export type WatchFilter = 'all' | 'watched' | 'unwatched'
export type StatusFilter = 'all' | 'active' | 'seeding' | 'paused' | 'error'

const SORTS: DownloadSort[] = ['added', 'name', 'size', 'state']
const WATCHES: WatchFilter[] = ['all', 'watched', 'unwatched']
const STATUSES: StatusFilter[] = ['all', 'active', 'seeding', 'paused', 'error']

/** The section of a download (shared with «Файлы»). */
export const segmentOf = (kind: Torrent['kind']): DownloadSegment | null => mediaSegmentOf(kind)

/** The status group of the «Состояние» filter (also the order of «По состоянию»). */
export function statusGroup(t: Torrent): Exclude<StatusFilter, 'all'> {
  if (t.status === 'error' || t.error || t.organizeError) return 'error'
  if (t.status === 'paused' || t.status === 'waiting_space' || t.dataMissing) return 'paused'
  if (t.status === 'seeding' || (t.direct && t.status === 'done')) return 'seeding'
  return 'active'
}

const STATE_RANK: Record<Exclude<StatusFilter, 'all'>, number> = { error: 0, active: 1, paused: 2, seeding: 3 }

const displayName = (t: Torrent) => t.title || t.name

/** A sorted copy: newest first, by name, largest first, or by state (then the most done first). */
export function sortTorrents(list: Torrent[], sort: DownloadSort, locale?: string): Torrent[] {
  const added = (t: Torrent) => Date.parse(t.addedAt) || 0
  const progress = (t: Torrent) => (t.size > 0 ? t.done / t.size : 0)
  const by: Record<DownloadSort, (a: Torrent, b: Torrent) => number> = {
    added: (a, b) => added(b) - added(a),
    name: (a, b) => displayName(a).localeCompare(displayName(b), locale, { sensitivity: 'base', numeric: true }),
    size: (a, b) => b.size - a.size,
    state: (a, b) => STATE_RANK[statusGroup(a)] - STATE_RANK[statusGroup(b)] || progress(b) - progress(a),
  }
  // ties keep the newest first
  return [...list].sort((a, b) => by[sort](a, b) || added(b) - added(a))
}

export type DownloadView = {
  segment: DownloadSegment
  sort: DownloadSort
  watch: WatchFilter
  status: StatusFilter
}

/** The downloads of a segment that pass the filters (watched/unwatched keeps only films and series the server marked). */
export function filterTorrents(list: Torrent[], v: Omit<DownloadView, 'sort'>): Torrent[] {
  return list.filter(
    (t) =>
      (v.segment === 'all' || segmentOf(t.kind) === v.segment) &&
      (v.status === 'all' || statusGroup(t) === v.status) &&
      (v.watch === 'all' || (t.watched !== undefined && t.watched === (v.watch === 'watched'))),
  )
}

const DOWNLOAD_VIEW: ViewSpec<DownloadView> = {
  segment: { param: 'kind', values: MEDIA_SEGMENTS },
  sort: { param: 'sort', values: SORTS },
  watch: { param: 'watch', values: WATCHES },
  status: { param: 'status', values: STATUSES },
}

/** The round glass button next to the switcher and its menu of sort and filters. */
function DownloadsMenu({ view, set, watchable }: { view: DownloadView; set: (p: Partial<DownloadView>) => void; watchable: boolean }) {
  const t = useT()
  const sortLabel: Record<DownloadSort, Key> = {
    added: 'downloads.sort.added',
    name: 'downloads.sort.name',
    size: 'downloads.sort.size',
    state: 'downloads.sort.state',
  }
  const watchLabel: Record<WatchFilter, Key> = {
    all: 'downloads.filter.watchAll',
    watched: 'downloads.filter.watched',
    unwatched: 'downloads.filter.unwatched',
  }
  const statusLabel: Record<StatusFilter, Key> = {
    all: 'downloads.filter.statusAll',
    active: 'downloads.filter.active',
    seeding: 'downloads.filter.seeding',
    paused: 'downloads.filter.paused',
    error: 'downloads.filter.error',
  }
  return (
    <ViewMenu
      label={t('downloads.view')}
      filtered={view.watch !== 'all' || view.status !== 'all'}
      resetLabel={t('downloads.filter.reset')}
      onReset={() => set({ watch: 'all', status: 'all' })}
      groups={[
        {
          heading: t('common.sort'),
          options: SORTS.map((s) => ({ value: s, label: t(sortLabel[s]), detail: s === 'state' ? t('downloads.sort.stateDetail') : undefined })),
          value: view.sort,
          onChange: (sort: DownloadSort) => set({ sort }),
        },
        {
          heading: t('downloads.filter.watch'),
          options: WATCHES.map((w) => ({ value: w, label: t(watchLabel[w]) })),
          value: view.watch,
          onChange: (watch: WatchFilter) => set({ watch }),
          hidden: !watchable && view.watch === 'all',
        },
        {
          heading: t('downloads.filter.status'),
          options: STATUSES.map((st) => ({ value: st, label: t(statusLabel[st]) })),
          value: view.status,
          onChange: (status: StatusFilter) => set({ status }),
        },
      ]}
    />
  )
}
export function Downloads() {
  const t = useT()
  const [state, setState] = useState<State>({ status: 'loading' })
  const [view, setView] = useUrlView(DOWNLOAD_VIEW)
  // The profile's watched flags come with GET /api/torrents only: kept for the
  // torrent.progress lists, which do not carry them.
  const watched = useRef(new Map<string, boolean>())
  const withWatched = (list: Torrent[]) =>
    list.map((x) => (x.watched === undefined && watched.current.has(x.infoHash) ? { ...x, watched: watched.current.get(x.infoHash) } : x))
  // The media disk is not mounted: every torrent is held (see the server).
  const [offline, setOffline] = useState('')
  useEffect(() => {
    const check = () =>
      core.getDisk().then(
        (d) => setOffline(d.offline ?? ''),
        () => {},
      )
    void check()
    const id = window.setInterval(() => void check(), DISK_CHECK_MS) // the banner appears and clears while the page is open
    return () => window.clearInterval(id)
  }, [])
  const load = useCallback((quiet = false) => {
    if (!quiet) setState({ status: 'loading' })
    getTorrents().then(
      (list) => {
        watched.current = new Map(list.filter((x) => x.watched !== undefined).map((x) => [x.infoHash, x.watched!]))
        setState({ status: 'ready', list })
      },
      (e: unknown) => !quiet && setState({ status: 'error', message: e instanceof ApiError ? e.message : t('common.loadFailed') }),
    )
  }, [])
  useEffect(() => {
    setTitle(t('common.downloads'))
    load()
  }, [load, t])
  // one function for every row, so a row whose torrent did not change skips the tick (DownloadRow is memo)
  const reload = useCallback(() => load(true), [load])
  useEvents(
    (e) => {
      if (Array.isArray(e.data)) setState({ status: 'ready', list: withWatched(e.data as Torrent[]) })
    },
    ['addon.torrents.admin.progress'],
  )

  const all = state.status === 'ready' ? state.list : []
  // the summary counts every download, whatever the section
  const loading = all.filter((t) => t.status === 'downloading' || t.status === 'queued')
  const directDone = all.filter((t) => t.direct && t.status === 'done')
  const waiting = all.filter((t) => t.status === 'paused' || t.status === 'waiting_space')
  const seedingAll = all.filter((t) => t.status === 'seeding' && !t.parked)
  const parkedAll = all.filter((t) => t.status === 'seeding' && t.parked)

  // the SDK's helper names «all» in the core's words; this page says it in its own
  const segments = useMediaSegments(all.map((x) => x.kind), view.segment, 'mfiles.seg.all').map((s) =>
    s.value === 'all' ? { ...s, label: t('downloads.seg.all') } : s,
  )
  const inSegment = filterTorrents(all, { segment: view.segment, watch: 'all', status: 'all' })
  const watchable = inSegment.some((x) => x.watched !== undefined)
  const list = sortTorrents(filterTorrents(all, view), view.sort, locale())
  const active = list.filter((t) => t.status !== 'seeding')
  const seeding = list.filter((t) => t.status === 'seeding' && !t.parked)
  const parked = list.filter((t) => t.status === 'seeding' && t.parked)
  const filtered = view.watch !== 'all' || view.status !== 'all'

  return (
    <section aria-labelledby="dl-title">
      <header className={m.header}>
        <div>
          <h2 id="dl-title" className={m.title}>
            {t('common.downloads')}
          </h2>
          {state.status === 'ready' && all.length > 0 && (
            <p className={m.subtitle}>
              {[
                loading.length > 0 ? t('downloads.sum.loading', { n: loading.length }) : '',
                waiting.length > 0 ? t('downloads.sum.waiting', { n: waiting.length }) : '',
                directDone.length > 0 ? t('downloads.sum.done', { n: directDone.length }) : '',
                seedingAll.length > 0 || parkedAll.length === 0 ? t('downloads.sum.seeding', { n: seedingAll.length }) : '',
                parkedAll.length > 0 ? t('downloads.sum.done', { n: parkedAll.length }) : '',
              ]
                .filter(Boolean)
                .join(' · ')}
            </p>
          )}
        </div>
      </header>
      {offline && (
        <div className={m.problem} role="alert">
          <Icon name="info" size={18} /> {offline}
        </div>
      )}
      {state.status === 'loading' && (
        <div className={m.spinnerRow} role="status">
          <span className={m.spinner} aria-hidden /> {t('downloads.loading')}
        </div>
      )}
      {state.status === 'error' && (
        <div className={m.problem} role="alert">
          <Icon name="cloud" size={18} /> {state.message}
          <Button size="small" variant="secondary" onClick={() => load()}>
            {t('common.retry')}
          </Button>
        </div>
      )}
      {state.status === 'ready' && all.length === 0 && (
        <div className={m.empty}>
          <Icon name="download" size={40} strokeWidth={1.4} />
          <h3>{t('downloads.empty')}</h3>
          <p>{t('downloads.emptyHint')}</p>
        </div>
      )}
      {state.status === 'ready' && all.length > 0 && (
        <ViewToolbar>
          <SegmentedControl label={t('downloads.seg.label')} segments={segments} value={view.segment} onChange={(segment) => setView({ segment })} />
          <DownloadsMenu view={view} set={setView} watchable={watchable} />
        </ViewToolbar>
      )}
      {state.status === 'ready' && all.length > 0 && list.length === 0 && (
        <div className={m.empty}>
          <Icon name={mediaSegmentIcon(view.segment, 'download')} size={40} strokeWidth={1.4} />
          <h3>{t('downloads.emptySection')}</h3>
          {filtered && (
            <>
              <p>{t('downloads.emptyFiltered')}</p>
              <Button size="small" variant="secondary" onClick={() => setView({ watch: 'all', status: 'all' })}>
                {t('downloads.filter.reset')}
              </Button>
            </>
          )}
        </div>
      )}
      {active.length > 0 && (
        <GroupedSection title={t('common.downloads')}>
          {active.map((t) => (
            <DownloadRow key={t.infoHash} t={t} onChanged={reload} />
          ))}
        </GroupedSection>
      )}
      {seeding.length > 0 && (
        <GroupedSection title={t('downloads.seedingTitle')} footer={t('downloads.seedingFoot')}>
          {seeding.map((t) => (
            <DownloadRow key={t.infoHash} t={t} onChanged={reload} />
          ))}
        </GroupedSection>
      )}
      {parked.length > 0 && (
        <GroupedSection title={t('downloads.parkedTitle')} footer={t('downloads.parkedFoot')}>
          {parked.map((t) => (
            <DownloadRow key={t.infoHash} t={t} onChanged={reload} />
          ))}
        </GroupedSection>
      )}
    </section>
  )
}
