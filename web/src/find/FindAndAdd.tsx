// «Найти и добавить» on «Поиск» (the full build's; never a kid's: kids get
// the core's RequestSearch): a catalog chosen by the segmented control (TMDB,
// MusicBrainz, Open Library, or the manga name itself) for what is not on
// the server yet: films get «Смотреть ▾» (the addon's own way, then the
// watch ways of the profile's other addons) and the round download button (the
// release sheet), series the same two as links to the title page; albums and
// books «На сервер». Admins can pick the release by hand («Выбрать
// раздачу…» in the ▾ menu, «Выбрать релиз…» for albums, where each torrent
// also offers «Выбрать треки…»: some songs only, the searched one checked;
// its «На сервер» opens the track step with the requested album checked
// unless the release is that album alone, which goes whole at once; an
// album the library has gets «Выбрать треки…» alone, to add songs to it). The catalog lives in the URL (?dk=).
import {
  ApiError,
  cardHref,
  core,
  DownloadButton,
  Icon,
  KIND_ICON,
  kindLabel,
  locale,
  titlePath,
  ui,
  useDebounced,
  useWatchWays,
  WatchButton,
  type Found,
  type SearchBlock,
  type WatchWay,
} from '@antresol/addon-sdk'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router'
import { discover, ID } from '../api'
import { useT, type Key } from '../i18n'
import { DownloadControl } from '../shared/DownloadControl'
import { releaseLink, ReleaseTable } from '../shared/ReleaseTable'
import { isMediaKind, useAcquire, type AcquireTarget } from '../shared/useAcquire'
import { useTitleProgress } from '../shared/useTitleProgress'
import { StalledNote } from '../StalledNote'
import type { Candidate, DiscoverKind, Release, ReleaseFilesRequest } from '../types'
import { TrackPicker } from './TrackPicker'
import styles from './FindAndAdd.module.css'

const { Button, ButtonLink, Sheet } = ui
const imageUrl = core.imageUrl

/** The search page's filter, as «Поиск» hands it to its blocks. */
export type SearchScope = Parameters<SearchBlock['element']>[0]['scope']

/** What «Найти и добавить» says it searches, next to its title. */
const DISCOVER_LABEL: Record<DiscoverKind, Key> = {
  video: 'search.discover.video',
  music: 'common.music',
  book: 'common.books',
  audiobook: 'common.audiobooks',
  comic: 'common.kind.manga',
}

/** Where an album or book that is already on the server lives. */
function libraryPath(c: Candidate): string | undefined {
  if (c.libraryItemId == null) return undefined
  const kind = c.kind === 'music' ? 'album' : c.kind
  return cardHref({ id: c.libraryItemId, kind, title: c.title, subtitle: '', poster: '', backdrop: '', logo: '', progress: 0, onServer: true })
}

/** The catalog of «Найти и добавить» that fits a scope. */
/** The scopes that take one kind of the video catalog's answer, and what the section is then called. */
const SCOPE_KIND: Partial<Record<SearchScope, string>> = { movies: 'movie', series: 'series' }
const SCOPE_LABEL: Partial<Record<SearchScope, Key>> = { movies: 'common.movies', series: 'common.series' }
const SCOPE_DISCOVER: Record<SearchScope, DiscoverKind> = { all: 'video', movies: 'video', series: 'video', music: 'music', books: 'book', audiobooks: 'audiobook', comics: 'comic' }

interface CandidateGroup {
  key: string
  title?: { key: Key; args?: Record<string, string> }
  items: Candidate[]
}

/**
 * Music whose query names an artist: «Альбомы Metallica» (the server lists
 * them first, byArtist set), then «Другие совпадения»; otherwise one
 * untitled list.
 */
export function candidateGroups(items: Candidate[]): CandidateGroup[] {
  if (items.length === 0) return []
  const artist = items[0]?.byArtist
  if (!artist) return [{ key: 'all', items }]
  const mine = items.filter((c) => c.byArtist)
  const rest = items.filter((c) => !c.byArtist)
  const groups: CandidateGroup[] = [{ key: 'artist', title: { key: 'search.artistAlbums', args: { artist } }, items: mine }]
  if (rest.length > 0) groups.push({ key: 'rest', title: { key: 'search.otherMatches' }, items: rest })
  return groups
}

/** The «Найти и добавить» section under the library results. */
export function FindAndAdd({ text, q, scope, nothing, admin, kind }: { text: string; q: string; scope: SearchScope; nothing: boolean; admin: boolean; kind?: DiscoverKind }) {
  const t = useT()
  // the page's one filter picks the catalog too (`kind`: an empty page's ?dk= asked for one)
  const dk = kind ?? SCOPE_DISCOVER[scope]
  const dq = useDebounced(text.trim(), 600)
  const [found, setFound] = useState<Found<Candidate[]>>({ status: 'idle' })

  useEffect(() => {
    if (dq.length < 2) return setFound({ status: 'idle' })
    const ac = new AbortController()
    setFound({ status: 'loading' })
    discover(dq, dk, ac.signal).then(
      // films already on the server are found above; albums and books say «Уже в медиатеке»
      (items) => setFound({ status: 'ready', items: dk === 'video' ? items.filter((c) => !c.inLibrary) : items }),
      (e: unknown) => !ac.signal.aborted && setFound({ status: 'error', message: e instanceof ApiError ? e.message : t('search.discoverUnavailable') }),
    )
    return () => ac.abort()
  }, [dq, dk])

  // films and series come from one catalog: «Фильмы» / «Сериалы» narrow its
  // answer here (no new request), and the title says so
  const wanted = SCOPE_KIND[scope]
  const items = found.status === 'ready' ? (wanted ? found.items.filter((c) => c.kind === wanted) : found.items) : []
  // nothing here either: the page's one empty state says it, not a second, empty section
  if (nothing && found.status === 'ready' && items.length === 0) return null
  return (
    <section className={styles.section} aria-labelledby="find-add">
      <div className={styles.discoverHeader}>
        <h2 id="find-add" className={styles.sectionTitle}>
          {t('search.findAndAdd')}
        </h2>
        <span className={styles.discoverWhere}>{t(SCOPE_LABEL[scope] ?? DISCOVER_LABEL[dk])}</span>
      </div>
      {(found.status === 'loading' || (found.status === 'idle' && dq !== q)) && <CandidateSkeleton square={dk === 'music'} />}
      {found.status === 'error' && (
        <p className={styles.problem} role="status">
          <Icon name="cloud" size={18} /> {found.message}
        </p>
      )}
      {found.status === 'ready' && items.length === 0 && (
        <p className={styles.muted}>{t('search.discoverNothingMore')}</p>
      )}
      {found.status === 'ready' &&
        candidateGroups(items).map((g) => (
          <div key={g.key} className={styles.candidateGroup}>
            {g.title && <h3 className={styles.groupTitle}>{t(g.title.key, g.title.args)}</h3>}
            <ul className={styles.candidates}>
              {g.items.map((c) =>
                isMediaKind(c.kind) ? (
                  <MediaCandidateRow key={`${c.kind}-${c.extId || c.title}`} c={c} admin={admin} query={dq} />
                ) : (
                  <CandidateRow key={`${c.kind}-${c.tmdbId}`} c={c} admin={admin} />
                ),
              )}
            </ul>
          </div>
        ))}
    </section>
  )
}

/**
 * A TMDB film or series: a film gets «Смотреть ▾» and the round download
 * button (the release sheet), a series «Выбрать серию» and a download link,
 * both to its page — a series goes to the server season by season.
 */
function CandidateRow({ c, admin }: { c: Candidate; admin: boolean }) {
  const t = useT()
  const { state, run, reset } = useAcquire()
  const [sheet, setSheet] = useState<'stream' | 'download' | null>(null)
  // «Выбрать другую раздачу…»: the picked download replaces the running one
  const [replacing, setReplacing] = useState(false)
  const acq = { state, run, reset }
  const poster = imageUrl(c.poster, 480)
  const target: AcquireTarget = { tmdbId: c.tmdbId, kind: c.kind, title: c.title, poster: c.poster }
  const busy = state.status === 'busy' ? state.mode : null
  const series = c.kind === 'series'
  const progress = useTitleProgress(series ? null : { kind: c.kind, tmdbId: c.tmdbId })
  // the addon's own way; the other addons' come after it (FilmWatch)
  const own: WatchWay = { id: 'torrent', label: t('item.way.torrent'), run: () => void run(target, 'stream') }

  // a started download: the sheet closes and the ring takes over at once
  const started = (ok: boolean) => {
    if (!ok) return
    progress.watch()
    setSheet(null)
  }
  const openSheet = (mode: 'stream' | 'download', replaceRunning = false) => {
    reset()
    setReplacing(replaceRunning)
    setSheet(mode)
  }

  return (
    <li className={styles.candidate}>
      <Link to={titlePath(c)} className={styles.poster} aria-label={c.title}>
        {poster ? <img src={poster} alt="" loading="lazy" /> : <Icon name={series ? 'tv' : 'film'} size={28} />}
      </Link>
      <div className={styles.info}>
        <p className={styles.kicker}>{[kindLabel(c.kind), c.year || ''].filter(Boolean).join(' · ')}</p>
        <Link to={titlePath(c)} className={styles.name}>
          {c.title}
        </Link>
        {c.overview && <p className={styles.overview}>{c.overview}</p>}
      </div>
      <div className={styles.footer}>
        <div className={styles.buttons}>
          {series ? (
            <>
              <ButtonLink to={titlePath(c)} size="small" icon="play">
                {t('search.pickEpisode')}
              </ButtonLink>
              <DownloadButton state={{ kind: 'idle' }} size="small" to={titlePath(c)} />
            </>
          ) : (
            <>
              <FilmWatch
                c={c}
                own={own}
                extra={[{ label: t('item.pickReleaseWatch'), run: () => openSheet('stream') }]}
                loading={busy === 'stream'}
                disabled={!!busy}
              />
              <DownloadControl
                state={busy === 'download' ? { kind: 'starting' } : progress.state}
                query={{ kind: c.kind, tmdbId: c.tmdbId }}
                size="small"
                disabled={busy === 'stream'}
                onPick={() => openSheet('download')}
                onReplace={() => openSheet('download', true)}
                onCancelled={progress.refresh}
              />
            </>
          )}
        </div>
        {busy === 'stream' && <p className={styles.note}>{t('search.preparing')}</p>}
        {!series && progress.state.kind === 'active' && busy !== 'download' && (
          <StalledNote stalled={progress.state.stalled} hint={progress.state.hint} onReplace={() => openSheet('download', true)} />
        )}
        <AcquireStatus state={state} picking={!!sheet} />
      </div>
      {sheet && <VideoReleaseSheet c={c} mode={sheet} replacing={replacing} admin={admin} acq={acq} onStarted={started} onClose={() => setSheet(null)} />}
    </li>
  )
}

/**
 * A film row's «Смотреть ▾»: the addon's own way, then the watch ways of
 * the profile's other addons (the SDK's useWatchWays: each answers its own
 * `useAvailable`, and joins the menu when it does), then the extras. A
 * component of its own so a series row, which has no «Смотреть ▾», asks no
 * addon.
 */
function FilmWatch({
  c,
  own,
  extra,
  loading,
  disabled,
}: {
  c: Candidate
  own: WatchWay
  extra: { label: string; run: () => void }[]
  loading: boolean
  disabled: boolean
}) {
  const t = useT()
  const all = useWatchWays({ kind: 'movie', tmdbId: c.tmdbId, title: c.title, poster: c.poster })
  // useWatchWays lists this addon's own ways too (`<id>:…`): the row has its own already
  const others = all.filter((w) => !w.id.startsWith(`${ID}:`))
  return (
    <WatchButton
      label={t('comp.action.watch')}
      ways={[own, ...others]}
      current={own.id}
      extra={extra}
      size="small"
      loading={loading}
      disabled={disabled}
    />
  )
}

type Acquire = Pick<ReturnType<typeof useAcquire>, 'state' | 'run' | 'reset'>

/**
 * The release sheet of a film (of the search results, or of «Одобрить» in
 * «Запросы»): «Лучший релиз» and the table; a started download calls
 * onStarted(true). acq is the caller's useAcquire (its row shows the result).
 */
export function VideoReleaseSheet({
  c,
  mode,
  replacing = false,
  admin,
  acq,
  onStarted,
  onClose,
}: {
  c: Pick<Candidate, 'tmdbId' | 'kind' | 'title' | 'year' | 'poster'>
  mode: 'stream' | 'download'
  replacing?: boolean
  admin: boolean
  acq: Acquire
  onStarted: (ok: boolean) => void
  onClose: () => void
}) {
  const t = useT()
  const { state, run } = acq
  const [picked, setPicked] = useState<{ link: string; mode: 'stream' | 'download' } | null>(null)
  const target: AcquireTarget = { tmdbId: c.tmdbId, kind: c.kind, title: c.title, poster: c.poster }
  const busy = state.status === 'busy' ? state.mode : null
  const pick = (r: Release, m: 'stream' | 'download') => {
    setPicked({ link: releaseLink(r), mode: m })
    void run(target, m, { releaseLink: releaseLink(r), sourceId: r.sourceId, direct: r.direct, replace: m === 'download' && replacing })
      .then((ok) => m === 'download' && onStarted(ok))
      .finally(() => setPicked(null))
  }
  return (
    <Sheet
      title={t('search.releasesTitle', { title: `${c.title}${c.year ? ` (${c.year})` : ''}` })}
      message={replacing ? t('item.download.replaceHint') : t('search.pickHint.video')}
      role="dialog"
      size="large"
      dismissOnOutside
      onCancel={onClose}
    >
      <PickError state={state} />
      <ReleaseTable
        tmdbId={c.tmdbId}
        kind={c.kind}
        mode={mode}
        // a series has no best pick: the server takes a series season by season
        best={mode === 'download' && c.kind !== 'series' ? { label: t('item.bestRelease'), run: () => void run(target, 'download', replacing ? { releaseLink: '', replace: true } : undefined).then(onStarted), busy: busy === 'download' && !picked } : undefined}
        busy={picked}
        onPick={pick}
        admin={admin}
      />
    </Sheet>
  )
}

const PICK_HINT: Record<string, Key> = {
  music: 'search.pickHint.music',
  book: 'search.pickHint.book',
  audiobook: 'search.pickHint.audiobook',
  comic: 'search.pickHint.comic',
}

/** Folded for comparing a query with a title: case, punctuation and spacing ignored. */
const fold = (s: string) =>
  s
    .toLocaleLowerCase(locale())
    .replace(/ё/g, 'е')
    .replace(/[^\p{L}\p{N}]+/gu, ' ')
    .trim()

/**
 * The song a music search was for: the catalog's matched song, else the
 * query itself unless it names the album or its artist (the server matches
 * it against the release's track titles, so a query that is no song marks
 * nothing).
 */
export function searchedSong(c: Pick<Candidate, 'title' | 'subtitle' | 'matchedTrack'>, query: string): string {
  if (c.matchedTrack) return c.matchedTrack
  const q = fold(query)
  const artist = fold(c.subtitle ?? '')
  const album = fold(c.title)
  if (!q || q === album || q === artist || q === `${artist} ${album}` || q === `${album} ${artist}`) return ''
  return query.trim()
}

/** An album (MusicBrainz), a book or audiobook (Open Library) or a manga: «На сервер» only. */
function MediaCandidateRow({ c, admin, query }: { c: Candidate; admin: boolean; query: string }) {
  const t = useT()
  const { state, run, reset } = useAcquire()
  const [picking, setPicking] = useState(false)
  const [failed, setFailed] = useState(false)
  const poster = imageUrl(c.poster, 480)
  const artist = c.kind === 'comic' ? '' : (c.subtitle ?? '')
  const target = mediaTarget(c)
  const busy = state.status === 'busy'
  const owned = c.inLibrary ? libraryPath(c) : undefined

  return (
    <li className={styles.candidate}>
      <span className={`${styles.poster} ${c.kind === 'music' ? styles.square : ''}`}>
        {poster && !failed ? (
          <img src={poster} alt="" loading="lazy" onError={() => setFailed(true)} />
        ) : (
          <Icon name={KIND_ICON[c.kind] ?? 'book'} size={28} />
        )}
      </span>
      <div className={styles.info}>
        <p className={styles.kicker}>{[kindLabel(c.kind), c.year || ''].filter(Boolean).join(' · ')}</p>
        <p className={styles.name}>{c.title}</p>
        {artist && <p className={styles.byline}>{artist}</p>}
        {c.kind === 'music' && c.matchedTrack && <p className={styles.matched}>{t('search.matchedSong', { title: c.matchedTrack })}</p>}
      </div>
      <div className={styles.footer}>
        <div className={styles.buttons}>
          {owned ? (
            <ButtonLink to={owned} size="small" variant="secondary">
              {t('search.inLibrary')}
            </ButtonLink>
          ) : (
            <Button size="small" variant="secondary" icon="download" loading={busy} disabled={busy} onClick={() => void run(target, 'download')}>
              {t('search.toServer')}
            </Button>
          )}
          {admin && (!owned || c.kind === 'music') && (
            <Button size="small" variant="plain" disabled={busy} onClick={() => (reset(), setPicking(true))}>
              {owned ? t('comp.release.pickTracks') : t('search.pickRelease')}
            </Button>
          )}
        </div>
        <AcquireStatus state={state} picking={picking} />
      </div>
      {picking && <MediaReleaseSheet c={c} query={query} admin={admin} owned={!!owned} acq={{ state, run, reset }} onClose={() => setPicking(false)} />}
    </li>
  )
}

/** What «На сервер» sends for an album, book, audiobook or manga. */
function mediaTarget(c: Candidate): AcquireTarget {
  const artist = c.kind === 'comic' ? '' : (c.subtitle ?? '')
  return { tmdbId: 0, kind: c.kind, title: c.title, extId: c.extId, artist, year: c.year, aliases: c.aliases, poster: c.poster }
}

/**
 * «Выбрать релиз…» of an album, book, audiobook or manga (of the search
 * results, or of «Одобрить» in «Запросы»): the release table, and for music
 * the track step in the same sheet. It closes once the download started.
 * owned: the library has the album (only «Выбрать треки…», to add songs);
 * best: a «Лучший релиз» row on top (the server picks, as «На сервер» does).
 */
export function MediaReleaseSheet({ c, query, admin, owned, best = false, acq, onClose }: { c: Candidate; query: string; admin: boolean; owned: boolean; best?: boolean; acq: Acquire; onClose: () => void }) {
  const t = useT()
  const { state, run, reset } = acq
  const [picked, setPicked] = useState<{ link: string; mode: 'stream' | 'download' } | null>(null)
  const artist = c.kind === 'comic' ? '' : (c.subtitle ?? '')
  const target = mediaTarget(c)
  const busy = state.status === 'busy'

  // Music: «Выбрать треки…» on a release opens the track step in the same sheet.
  const [tracksOf, setTracksOf] = useState<Release | null>(null)
  const [tracksPreset, setTracksPreset] = useState<'searched' | 'album'>('searched')
  const song = c.kind === 'music' ? searchedSong(c, query) : ''
  const tracksRequest = useMemo<ReleaseFilesRequest | null>(
    () =>
      tracksOf
        ? { sourceId: tracksOf.sourceId, releaseLink: releaseLink(tracksOf), title: c.title, artist, year: c.year ?? 0, aliases: c.aliases ?? [], ...(song ? { song } : {}) }
        : null,
    [tracksOf, c.title, artist, c.year, c.aliases, song],
  )
  // «Назад» gives focus back to the release's «На сервер» (it was hidden under the track step).
  const tableRef = useRef<HTMLDivElement>(null)
  const returnFocus = useRef<HTMLElement | null>(null)
  useEffect(() => {
    if (tracksOf || !returnFocus.current) return
    const el = returnFocus.current
    returnFocus.current = null
    if (tableRef.current?.contains(el)) el.focus()
    else tableRef.current?.querySelector<HTMLElement>('button')?.focus()
  }, [tracksOf])

  const pickTracksOf = (r: Release, preset: 'searched' | 'album' = 'searched') => {
    reset()
    const active = document.activeElement
    returnFocus.current = active instanceof HTMLElement ? active : null
    setTracksPreset(preset)
    setTracksOf(r)
  }
  // an album torrent's «На сервер»: a discography must not come whole (GBs,
  // every album imported) — the track step with the requested album checked
  const pick = (r: Release) => (c.kind === 'music' && !r.direct ? pickTracksOf(r, 'album') : pickWhole(r))
  // the whole release, as the server would download it
  const pickWhole = (r: Release) => {
    setPicked({ link: releaseLink(r), mode: 'download' })
    void run(target, 'download', { releaseLink: releaseLink(r), sourceId: r.sourceId, direct: r.direct, releaseTitle: r.title }).finally(() => setPicked(null))
  }
  const pickTracks = (r: Release, files: number[]) =>
    void run(target, 'download', { releaseLink: releaseLink(r), sourceId: r.sourceId, releaseTitle: r.title, files })
  const closed = useRef(onClose)
  closed.current = onClose
  useEffect(() => {
    if (state.status === 'done') closed.current()
  }, [state.status])

  return (
    <Sheet
      title={t('search.releasesTitle', { title: `${artist ? `${artist} — ` : ''}${c.title}${c.year ? ` (${c.year})` : ''}` })}
      message={tracksOf || !PICK_HINT[c.kind] ? undefined : t(PICK_HINT[c.kind]!)}
      role="dialog"
      size="large"
      // the track step holds a selection: a stray click must not drop it
      dismissOnOutside={!tracksOf}
      onCancel={onClose}
    >
      {/* the track step shows a failed confirm in its own footer */}
      {!tracksOf && <PickError state={state} />}
      {/* kept mounted under the track step, so «Назад» does not search again */}
      <div hidden={!!tracksOf} ref={tableRef}>
        <ReleaseTable
          kind={c.kind}
          extId={c.extId}
          title={c.title}
          artist={artist}
          year={c.year}
          aliases={c.aliases}
          busy={picked}
          onPick={pick}
          onPickTracks={c.kind === 'music' ? pickTracksOf : undefined}
          tracksOnly={owned}
          best={best ? { label: t('item.bestRelease'), run: () => void run(target, 'download'), busy: busy && !picked && !tracksOf } : undefined}
          admin={admin}
        />
      </div>
      {tracksOf && tracksRequest && (
        <TrackPicker
          request={tracksRequest}
          busy={busy}
          error={state.status === 'error' ? state.message : undefined}
          onBack={() => (reset(), setTracksOf(null))}
          onConfirm={(files) => pickTracks(tracksOf, files)}
          preset={tracksPreset}
          onWhole={() => void run(target, 'download', { releaseLink: releaseLink(tracksOf), sourceId: tracksOf.sourceId, releaseTitle: tracksOf.title })}
        />
      )}
    </Sheet>
  )
}

type AcquireState = ReturnType<typeof useAcquire>['state']

function AcquireStatus({ state, picking }: { state: AcquireState; picking: boolean }) {
  const t = useT()
  if (state.status === 'done')
    return (
      <p className={styles.done} role="status">
        <Icon name="checkCircle" size={16} /> {state.message}
      </p>
    )
  if (state.status === 'error' && !picking)
    return (
      <p className={styles.problem} role="alert">
        {state.message}
        {state.otherFormat && (
          <>
            {' '}
            <Button size="small" variant="secondary" onClick={state.otherFormat}>
              {t('search.otherFormat')}
            </Button>
          </>
        )}
      </p>
    )
  return null
}

function PickError({ state }: { state: AcquireState }) {
  if (state.status !== 'error') return null
  return (
    <p className={styles.problem} role="alert">
      {state.message}
    </p>
  )
}

function CandidateSkeleton({ square }: { square: boolean }) {
  const t = useT()
  return (
    <ul className={styles.candidates} aria-busy="true" aria-label={t('search.searching')}>
      {[0, 1, 2].map((i) => (
        <li key={i} className={styles.candidate}>
          <div className={`${styles.poster} ${square ? styles.square : ''} ${styles.skeleton}`} />
          <div className={styles.info}>
            <div className={styles.skeletonLine} style={{ width: '30%' }} />
            <div className={styles.skeletonLine} style={{ width: '55%', height: 18 }} />
            <div className={styles.skeletonLine} style={{ width: '80%' }} />
          </div>
        </li>
      ))}
    </ul>
  )
}
