// «Скачать» of a movie or series that is not (all) on the server: the sheet
// an item action returns and «Добавить сезон…» opens. It asks the
// quality first (the qualities the search finds as it goes; 4K only when a
// source has it), with the season for a series («Все сезоны» when TMDB knows
// the list); «Скачать» downloads the server's pick in that quality,
// «Выбрать раздачу…» opens the release table (its first row the server's
// pick, «Закрепить» per row). `replace` (from «Загрузки»: ?replace=1) opens
// the release table at once and the picked download replaces the running one.
import { ApiError, core, Icon, ui, useUser } from '@antresol/addon-sdk'
import { useEffect, useRef, useState } from 'react'
import { cancelTitleDownload, pinRelease, streamReleases } from '../api'
import { useT } from '../i18n'
import { usePinned } from '../pinned/PinnedRelease'
import { releaseLink, ReleaseTable } from '../shared/ReleaseTable'
import { useAcquire, type AcquireTarget, type ReleaseChoice } from '../shared/useAcquire'
import type { FollowResolution, Release, ReleaseSearch } from '../types'
import styles from './TitlePanels.module.css'

const { SegmentedControl, Select, Sheet } = ui

/** When TMDB has no season list: let people pick what they know. */
export const FALLBACK_SEASONS = /* @__PURE__ */ Array.from({ length: 30 }, (_, i) => ({ number: i + 1, episodes: 50 }))
/** «Все сезоны» in the season picker. */
export const ALL = 0

/** The download qualities a search may find. */
const ASK_QUALITIES: FollowResolution[] = ['720p', '1080p', '2160p']
/** Offered always (the server falls back to the best other with a note); 4K only when a search found it. */
const BASE_QUALITIES: FollowResolution[] = ['720p', '1080p']
/** Preselected: never 4K (no surprise remux). */
const DEFAULT_QUALITY: FollowResolution = '1080p'

/** The qualities «Скачать» offers once the search found `found`. */
export function offeredQualities(found: FollowResolution[]): FollowResolution[] {
  return found.includes('2160p') ? [...BASE_QUALITIES, '2160p'] : BASE_QUALITIES
}

/** The qualities of a search's releases, of those «Скачать» offers ([] when none says). */
export function searchQualities(s: Pick<ReleaseSearch, 'releases'>): FollowResolution[] {
  const have = new Set(s.releases.map((r) => `${r.resolution ?? 0}p`))
  return ASK_QUALITIES.filter((q) => have.has(q))
}

export type Seasons = { number: number; episodes: number }[]

// A download this sheet started, for the page's ring of the same title
// (the panel's; the sheet is an item action's, drawn elsewhere).
const started = new Set<(key: string) => void>()
/** Calls `then` when a sheet starts a download of the title; returns the unsubscribe. */
export function onDownloadStarted(kind: string, tmdbId: number, then: () => void): () => void {
  const on = (key: string) => key === `${kind}:${tmdbId}` && then()
  started.add(on)
  return () => started.delete(on)
}

export interface DownloadSheetProps {
  kind: 'series' | 'movie'
  tmdbId: number
  title: string
  poster?: string
  backdrop?: string
  year?: number
  /** The seasons to offer (a library series: the ones not on the server); absent: TMDB's (asked here). */
  seasons?: Seasons
  /** «Все сезоны» when the season list is real; absent: when TMDB's list is known. */
  allowAll?: boolean
  /** The season to start with (`ALL` for «Все сезоны»). */
  season?: number
  /** «Выбрать другую раздачу…»: straight to the release table, replacing the running download. */
  replace?: boolean
  onClose: () => void
  /** A download started (the page's ring may follow it). */
  onStarted?: () => void
}

export function DownloadSheet({ kind, tmdbId, title, poster, backdrop, year, seasons: given, allowAll: allowGiven, season: first, replace = false, onClose, onStarted }: DownloadSheetProps) {
  const t = useT()
  const user = useUser()
  const series = kind === 'series'
  const { state, run, runSeasons, reset } = useAcquire()
  // the season list: the caller's, else TMDB's (the 1–30 fallback when it has none)
  const [known, setKnown] = useState<{ seasons: Seasons; real: boolean } | null>(given ? { seasons: given, real: allowGiven ?? given.length > 0 } : series ? null : { seasons: [], real: false })
  useEffect(() => {
    if (known) return
    const ac = new AbortController()
    core.getTitle('series', tmdbId, ac.signal).then(
      (ti) => setKnown({ seasons: ti.seasons ?? [], real: (ti.seasons ?? []).length > 0 }),
      () => !ac.signal.aborted && setKnown({ seasons: [], real: false }),
    )
    return () => ac.abort()
  }, [known, tmdbId])
  const seasons = known && known.seasons.length > 0 ? known.seasons : FALLBACK_SEASONS
  const allowAll = Boolean(known?.real)
  const [season, setSeason] = useState<number | null>(first ?? null)
  const current = season === ALL && allowAll && series ? ALL : season && seasons.some((s) => s.number === season) ? season : (seasons[0]?.number ?? 1)
  const all = series && current === ALL

  const [step, setStep] = useState<'ask' | 'releases' | 'done'>(replace ? 'releases' : 'ask')
  const [found, setFound] = useState<FollowResolution[]>([])
  const [quality, setQuality] = useState<FollowResolution>(DEFAULT_QUALITY)
  // the quality asked before the release table opened ('' when it was not)
  const [sheetQuality, setSheetQuality] = useState('')
  const [picked, setPicked] = useState<{ link: string; mode: 'stream' | 'download' } | null>(null)
  const [cancelError, setCancelError] = useState('')
  const [pinning, setPinning] = useState('')
  const [pinError, setPinError] = useState('')
  const pinned = usePinned(kind, tmdbId)

  // the search behind the quality step: each answer may add a quality (4K once a source has it)
  const searchKey = series && !all ? current : -1
  const search = useRef<AbortController | null>(null)
  useEffect(() => {
    if (step !== 'ask' || (series && !known)) return
    setFound([])
    const ac = new AbortController()
    search.current = ac
    const seen = (s: Pick<ReleaseSearch, 'releases'>) => {
      if (ac.signal.aborted) return
      const qs = searchQualities(s)
      setFound((had) => (qs.every((q) => had.includes(q)) ? had : ASK_QUALITIES.filter((q) => had.includes(q) || qs.includes(q))))
    }
    streamReleases({ kind, tmdbId, season: searchKey > 0 ? searchKey : undefined }, seen, ac.signal).then(seen, () => undefined)
    return () => ac.abort()
  }, [step, kind, tmdbId, searchKey, series, known])

  const target: AcquireTarget = { tmdbId, kind, title, poster, backdrop }
  const seasonTarget = series && !all ? { ...target, season: current } : target
  const busy = state.status === 'busy' ? state.mode : null
  const progressQuery = { kind, tmdbId, season: series && !all ? current : undefined }
  const begun = (ok: boolean) => {
    if (!ok) return
    for (const on of started) on(`${kind}:${tmdbId}`)
    onStarted?.()
    setStep('done')
  }
  const offered = offeredQualities(found)
  const chosen = offered.includes(quality) ? quality : DEFAULT_QUALITY
  const downloadQuality = () => {
    search.current?.abort()
    if (all) return void runSeasons(target, seasons.map((s) => s.number), chosen).then(begun)
    void run(seasonTarget, 'download', { releaseLink: '', quality: chosen }).then(begun)
  }
  const openReleases = () => {
    search.current?.abort()
    reset()
    setSheetQuality(chosen)
    setStep('releases')
  }
  // «Лучшая раздача» keeps the quality asked before the release table (sheetQuality)
  const best: ReleaseChoice | undefined = replace || sheetQuality ? { releaseLink: '', replace: replace || undefined, quality: sheetQuality || undefined } : undefined
  const downloadBest = () => {
    if (!all) return void run(seasonTarget, 'download', best).then(begun)
    // one download per season: the old ones go first (a replace flag would make each season cancel the one before)
    setCancelError('')
    const before = replace ? cancelTitleDownload(progressQuery).then(() => undefined) : Promise.resolve()
    void before.then(
      () => runSeasons(target, seasons.map((s) => s.number), sheetQuality).then(begun),
      (e: unknown) => setCancelError(e instanceof Error ? e.message : t('app.failedTryAgain')),
    )
  }
  const pick = (r: Release) => {
    setPicked({ link: releaseLink(r), mode: 'download' })
    void run(seasonTarget, 'download', { releaseLink: releaseLink(r), sourceId: r.sourceId, replace })
      .then(begun)
      .finally(() => setPicked(null))
  }
  const pinRow = (r: Release) => {
    const link = releaseLink(r)
    setPinning(link)
    setPinError('')
    pinRelease(kind, tmdbId, { sourceId: r.sourceId, link, details: r.details, releaseTitle: r.title }, { name: title, poster })
      .then(pinned.set, (e: unknown) => setPinError(e instanceof ApiError ? e.message : t('app.failedTryAgain')))
      .finally(() => setPinning(''))
  }

  const seasonPicker = series && (
    <div className={styles.episodePick}>
      <Select
        showLabel
        label={t('common.kind.season')}
        value={String(current)}
        options={[
          ...seasons.map((s) => ({ value: String(s.number), label: String(s.number) })),
          ...(allowAll && seasons.length > 1 ? [{ value: String(ALL), label: t('item.allSeasons') }] : []),
        ]}
        onChange={(v) => {
          setSeason(Number(v))
          reset()
        }}
      />
    </div>
  )

  if (step === 'done') {
    return (
      <Sheet title={t('item.download.quality.title')} role="dialog" onCancel={onClose} actions={[{ label: t('common.done'), kind: 'primary', run: onClose }]}>
        {state.status === 'done' && (
          <p className={styles.acquireDone} role="status">
            <Icon name="checkCircle" size={18} /> {state.message}
          </p>
        )}
      </Sheet>
    )
  }
  if (step === 'ask') {
    return (
      <Sheet
        title={t('item.download.quality.title')}
        message={t('item.download.quality.message')}
        role="dialog"
        busy={busy === 'download'}
        onCancel={onClose}
        actions={[
          { label: t('common.cancel'), kind: 'cancel', run: onClose },
          { label: t('item.download.quality.pick'), run: openReleases },
          { label: t('item.download.quality.go'), kind: 'primary', run: downloadQuality },
        ]}
      >
        {seasonPicker}
        <SegmentedControl
          label={t('item.follow.quality.label')}
          value={chosen}
          onChange={(v) => setQuality(v)}
          segments={offered.map((r) => ({ value: r, label: r === '2160p' ? '4K' : r }))}
        />
        {busy === 'download' && all && <p className={styles.acquireNote}>{t('item.searchingSeasons')}</p>}
        {state.status === 'error' && (
          <p className={styles.acquireError} role="alert">
            {state.message}
          </p>
        )}
      </Sheet>
    )
  }
  return (
    <Sheet
      title={t('search.releasesTitle', { title: `${title}${series ? (all ? t('item.releasesAllSeasons') : t('item.releasesSeason', { season: current })) : year ? ` (${year})` : ''}` })}
      message={replace ? t('item.download.replaceHint') : t('search.pickHint.video')}
      role="dialog"
      size="large"
      dismissOnOutside
      onCancel={onClose}
    >
      {replace && seasonPicker}
      {(state.status === 'error' || cancelError || pinError) && (
        <p className={styles.acquireError} role="alert">
          {cancelError || pinError || (state.status === 'error' && state.message)}
        </p>
      )}
      {(!series || known) && (
        <ReleaseTable
          tmdbId={tmdbId}
          kind={kind}
          season={series && !all ? current : undefined}
          mode="download"
          best={{ label: all ? t('item.bestEachSeason') : t('item.bestRelease'), run: downloadBest, busy: busy === 'download' && !picked }}
          busy={picked}
          onPick={pick}
          admin={Boolean(user?.isAdmin)}
          pin={pinned.available ? { onPin: pinRow, pinnedLink: pinned.pin?.link, busyLink: pinning || undefined } : undefined}
        />
      )}
      {busy === 'download' && all && <p className={styles.acquireNote}>{t('item.searchingSeasons')}</p>}
    </Sheet>
  )
}
