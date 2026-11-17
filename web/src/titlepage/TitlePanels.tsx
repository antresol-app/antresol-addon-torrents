// The addon's panel under the action row of an item's and a title's page
// (itemPanels; on a title page `item.id` is 0 and `title` the TMDB title):
// «Докачивать новые» / «Скачать, когда выйдет» (the follow from x/follow/
// by-tmdb), the household's pinned release, a stalled download's note,
// and on a library series «Добавить сезон…» (admins). Moved from the core's
// ItemDetailFull.tsx. The core draws none of
// it for a kid. The watch and download sheets of a title off the library
// are the addon's item actions (WatchSheet, DownloadSheet).
import { core, Icon, ui, useUser, type ItemDetail, type TitleInfo } from '@antresol/addon-sdk'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router'
import { getPin } from '../api'
import { useFollow } from '../follow/useFollow'
import { useT, type Key } from '../i18n'
import { PinnedRelease, usePinned } from '../pinned/PinnedRelease'
import { DownloadControl } from '../shared/DownloadControl'
import { useTitleProgress } from '../shared/useTitleProgress'
import { StalledNote } from '../StalledNote'
import type { FollowResolution } from '../types'
import { ALL, DownloadSheet, FALLBACK_SEASONS, onDownloadStarted, type Seasons } from './DownloadSheet'
import { WatchSheet } from './WatchSheet'
import styles from './TitlePanels.module.css'

const { Button, IconButton, SegmentedControl, Sheet, Toast } = ui

type TitleKind = 'series' | 'movie'

/** The title's TMDB id: the item's (a library title), else the title page's. */
const tmdbOf = (item: ItemDetail, title?: TitleInfo) => item.tmdbId || title?.tmdbId || 0

/**
 * What «Следить» follows: a series (on its page or in the library), a movie
 * only off the library (one in the library has nothing to wait for).
 */
export function followTarget(item: ItemDetail, title?: TitleInfo): { kind: TitleKind; tmdbId: number } | null {
  const id = tmdbOf(item, title)
  if (id <= 0) return null
  if (item.kind === 'series') return { kind: 'series', tmdbId: id }
  if (item.kind === 'movie' && item.id === 0) return { kind: 'movie', tmdbId: id }
  return null
}

/** The pinned release's title: a series, or a movie, with a TMDB id. */
const pinTarget = (item: ItemDetail, title?: TitleInfo): { kind: TitleKind; tmdbId: number } | null => {
  const id = tmdbOf(item, title)
  if (id <= 0) return null
  return item.kind === 'series' || item.kind === 'movie' ? { kind: item.kind === 'series' ? 'series' : 'movie', tmdbId: id } : null
}

const FOLLOW_HINT: Record<TitleKind, Key> = { series: 'item.follow.hintSeries', movie: 'item.follow.hintMovie' }
const FOLLOW_LABEL: Record<TitleKind, Key> = { series: 'item.follow.followSeries', movie: 'item.follow.followMovie' }
const FOLLOWING_LABEL: Record<TitleKind, Key> = { series: 'item.follow.followingSeries', movie: 'item.follow.followingMovie' }
const RESOLUTIONS: FollowResolution[] = ['', '720p', '1080p', '2160p']
/** The pin's quality as a choice of «Докачивать новые» ('' when it is none of them). */
const resolutionOf = (q: string | undefined): FollowResolution => RESOLUTIONS.find((r) => r !== '' && r === q) ?? ''

/**
 * «Докачивать новые» (a series: turning it on asks the quality, the pinned
 * release's by default) / «Скачать, когда выйдет» (a movie: on at once).
 * Nothing until the follow is known, nor when the server cannot follow.
 */
export function FollowButton({ kind, tmdbId }: { kind: TitleKind; tmdbId: number }) {
  const f = useFollow(kind, tmdbId)
  const t = useT()
  const [asking, setAsking] = useState(false)
  const [resolution, setResolution] = useState<FollowResolution>('')
  // a quality picked by hand is not overwritten by the pin's arriving late
  const chosen = useRef(false)
  if (!f.available) return null
  const label = t(FOLLOW_LABEL[kind])
  const click = () => {
    if (kind !== 'series' || f.following || f.undoable) return void f.toggle()
    chosen.current = false
    setResolution('')
    setAsking(true)
    getPin(kind, tmdbId).then(
      (p) => {
        if (!chosen.current) setResolution(resolutionOf(p?.quality))
      },
      () => undefined,
    )
  }
  return (
    <div className={styles.follow}>
      {f.followedBy ? (
        // another profile's follow: shown, changed only by its author or the admin
        <p className={`${styles.followedBy} glass`} aria-describedby={`follow-hint-${tmdbId}`}>
          <Icon name={f.following ? 'checkCircle' : 'eye'} size={20} />
          {t('item.follow.followedBy', { name: f.followedBy })}
          {!f.following && <span className={styles.followedOff}>{t('item.follow.off')}</span>}
        </p>
      ) : (
        // a round eye like the download button; on: the round toggles'
        // white «on» look, so the state reads at a glance
        <IconButton
          size="lg"
          icon="eye"
          // one name; aria-pressed says whether it is on
          toggled={f.following}
          label={label}
          tile
          tooltip={f.following ? t(FOLLOWING_LABEL[kind]) : label}
          aria-describedby={`follow-hint-${tmdbId}`}
          loading={f.busy}
          onClick={click}
        />
      )}
      {/* the button's description, for screen readers only: on screen it was clutter */}
      <p id={`follow-hint-${tmdbId}`} className="visually-hidden">
        {t(FOLLOW_HINT[kind])}
      </p>
      {f.error && (
        <p className={styles.acquireError} role="alert">
          {f.error}
        </p>
      )}
      {asking && (
        <Sheet
          title={t('item.follow.quality.title')}
          message={t('item.follow.quality.message')}
          role="dialog"
          onCancel={() => setAsking(false)}
          actions={[
            { label: t('common.cancel'), kind: 'cancel', run: () => setAsking(false) },
            {
              label: t('item.follow.quality.on'),
              kind: 'primary',
              run: () => {
                setAsking(false)
                void f.toggle(resolution)
              },
            },
          ]}
        >
          <SegmentedControl
            label={t('item.follow.quality.label')}
            value={resolution}
            onChange={(v) => {
              chosen.current = true
              setResolution(v)
            }}
            shortLabels
            segments={RESOLUTIONS.map((r) =>
              r === '' ? { value: r, label: t('item.follow.quality.default'), short: t('item.follow.quality.default.short') } : { value: r, label: r === '2160p' ? '4K' : r },
            )}
          />
        </Sheet>
      )}
      {/* stopping is reversible for a few seconds (no confirmation) */}
      <Toast action={f.undoable ? { label: t('item.undo'), onClick: f.undo } : undefined}>
        {f.undoable && (kind === 'series' ? t('item.follow.stoppedSeries') : t('item.follow.stoppedMovie'))}
      </Toast>
    </div>
  )
}

/** The pinned release of a title (none shown until the server answered). */
export function PinnedPanel({ kind, tmdbId, name, poster }: { kind: TitleKind; tmdbId: number; name?: string; poster?: string }) {
  const pinned = usePinned(kind, tmdbId)
  if (!pinned.available) return null
  return <PinnedRelease kind={kind} tmdbId={tmdbId} pin={pinned.pin} onChange={pinned.set} title={{ name, poster }} />
}

/** Seasons of TMDB `known` that are not on the server (no episodes on disk). */
export function missingSeasons(known: Seasons, onDisk: ItemDetail['seasons']): Seasons {
  const have = new Set(onDisk.filter((s) => s.episodes.length > 0).map((s) => s.number))
  return known.filter((s) => s.number > 0 && !have.has(s.number))
}

/** «Добавить сезон…» on a library series: one «Загрузить на сервер» for the seasons that are not on the server. */
export function AddSeason({ item, tmdbId }: { item: ItemDetail; tmdbId: number }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const [state, setState] = useState<{ status: 'loading' } | { status: 'ready'; seasons: Seasons; known: boolean }>({ status: 'loading' })
  useEffect(() => {
    if (!open) return
    setState({ status: 'loading' })
    const ac = new AbortController()
    core.getTitle('series', tmdbId, ac.signal).then(
      (ti) => setState({ status: 'ready', seasons: missingSeasons(ti.seasons ?? [], item.seasons), known: (ti.seasons ?? []).length > 0 }),
      // TMDB unavailable: let people pick a number (seasons on disk excluded)
      () => !ac.signal.aborted && setState({ status: 'ready', seasons: missingSeasons(FALLBACK_SEASONS, item.seasons), known: false }),
    )
    return () => ac.abort()
  }, [open, tmdbId, item.seasons])
  const [sheet, setSheet] = useState(false)
  if (!open) {
    return (
      <div>
        <Button size="small" variant="secondary" icon="plus" onClick={() => setOpen(true)}>
          {t('item.addSeason.aria')}
        </Button>
      </div>
    )
  }
  if (state.status === 'loading') {
    return (
      <p className={styles.acquireNote} role="status">
        {t('item.seasonsLoading')}
      </p>
    )
  }
  if (state.seasons.length === 0) {
    return (
      <p className={styles.acquireDone} role="status">
        <Icon name="checkCircle" size={18} /> {t('item.allSeasonsOnServer')}
      </p>
    )
  }
  return (
    <div className={styles.acquire}>
      <div>
        <Button size="large" variant="secondary" icon="download" onClick={() => setSheet(true)}>
          {t('item.downloadToServer')}
        </Button>
      </div>
      {sheet && (
        <DownloadSheet kind="series" tmdbId={tmdbId} title={item.title} poster={item.poster} backdrop={item.backdrop} seasons={state.seasons} allowAll={state.known} onClose={() => setSheet(false)} />
      )}
    </div>
  )
}

/**
 * A running download of a title off the library: the round ring with its
 * menu («Выбрать другую раздачу…», «Отменить загрузку»), and why it makes
 * no headway. ?replace=1 (from «Загрузки») opens «Выбрать другую раздачу…»
 * at once for ?season= (all: the whole-series pack). Nothing while no
 * download runs (the «Скачать» item action starts one).
 */
function TitleDownload({ kind, tmdbId, title }: { kind: TitleKind; tmdbId: number; title: TitleInfo }) {
  const [params, setParams] = useSearchParams()
  const [replacing, setReplacing] = useState<{ season?: number } | null>(null)
  const [picking, setPicking] = useState(false)
  useEffect(() => {
    if (params.get('replace') !== '1') return
    const s = params.get('season')
    setReplacing({ season: s === 'all' ? ALL : Number(s) || undefined })
    setParams(
      (p) => {
        const next = new URLSearchParams(p)
        next.delete('replace')
        next.delete('season')
        return next
      },
      { replace: true },
    )
  }, []) // once, on arrival
  // the title's downloads, every season (the ring of the whole title)
  const query = useMemo(() => ({ kind, tmdbId }), [kind, tmdbId])
  const progress = useTitleProgress(query)
  // a download the «Скачать» sheet started: the ring looks at once
  useEffect(() => onDownloadStarted(kind, tmdbId, progress.watch), [kind, tmdbId, progress.watch])
  const sheet = (props: { replace?: boolean; season?: number }) => (
    <DownloadSheet
      kind={kind}
      tmdbId={tmdbId}
      title={title.title}
      poster={title.poster}
      backdrop={title.backdrop}
      year={title.year || undefined}
      seasons={kind === 'series' ? (title.seasons ?? []) : undefined}
      allowAll={(title.seasons ?? []).length > 0}
      {...props}
      onClose={() => (setReplacing(null), setPicking(false))}
    />
  )
  const state = progress.state
  return (
    <>
      {state.kind !== 'idle' && (
        <div>
          <DownloadControl state={state} query={query} onPick={() => setPicking(true)} onReplace={() => setReplacing({})} onCancelled={progress.refresh} />
        </div>
      )}
      {state.kind === 'active' && <StalledNote stalled={state.stalled} hint={state.hint} onReplace={() => setReplacing({})} />}
      {replacing && sheet({ replace: true, season: replacing.season })}
      {picking && !replacing && sheet({})}
    </>
  )
}

/** The panel (itemPanels): what of it applies to this page. */
export function TitlePanels({ item, title }: { item: ItemDetail; title?: TitleInfo }) {
  const user = useUser()
  if (!user || user.isKid) return null
  const follow = followTarget(item, title)
  const pin = pinTarget(item, title)
  const onTitle = item.id === 0 && Boolean(title)
  const addSeason = item.id > 0 && item.kind === 'series' && (item.tmdbId ?? 0) > 0 && user.isAdmin
  if (!follow && !pin && !addSeason) return null
  return (
    <div className={styles.panel}>
      {follow && <FollowButton key={`${follow.kind}-${follow.tmdbId}`} kind={follow.kind} tmdbId={follow.tmdbId} />}
      {addSeason && <AddSeason item={item} tmdbId={item.tmdbId!} />}
      {onTitle && pin && title && <TitleDownload kind={pin.kind} tmdbId={pin.tmdbId} title={title} />}
      {pin && <PinnedPanel key={`${pin.kind}-${pin.tmdbId}`} kind={pin.kind} tmdbId={pin.tmdbId} name={item.title} poster={item.poster} />}
    </div>
  )
}

// --- the item actions of a title off the library (the core draws the sheet a run returns) ---

const isTitle = (it: ItemDetail) => it.id === 0 && (it.kind === 'movie' || it.kind === 'series') && (it.tmdbId ?? 0) > 0

/** «Смотреть сейчас» as a way of the core's «Смотреть ▾»: a title off the library an adult may stream. */
export const watchableOffLibrary = (it: ItemDetail) => isTitle(it) && it.actions.watchNow

/** «Скачать» as a round button: a title off the library an adult may download to the server. */
export const downloadable = (it: ItemDetail) => isTitle(it) && it.actions.toServer

const kindOf = (it: ItemDetail): TitleKind => (it.kind === 'series' ? 'series' : 'movie')

/** The watch way's run: the sheet that streams the title (a movie at once, a series' chosen episode). */
export const watchFromTitle = (it: ItemDetail, close: () => void) => (
  <WatchSheet kind={kindOf(it)} tmdbId={it.tmdbId ?? 0} title={it.title} poster={it.poster} year={it.year} onClose={close} />
)

/** «Скачать»'s run: the quality and release sheet. */
export const openDownload = (it: ItemDetail, close: () => void) => (
  <DownloadSheet kind={kindOf(it)} tmdbId={it.tmdbId ?? 0} title={it.title} poster={it.poster} backdrop={it.backdrop} year={it.year} onClose={close} />
)
