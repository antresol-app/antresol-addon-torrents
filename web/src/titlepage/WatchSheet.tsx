// «Смотреть сейчас» of a title that is not on the server: the sheet the
// addon's watch way returns (a way of the core's «Смотреть ▾»).
// A movie starts at once: from the household's pin when there is one
// (x/pinned/…/play), else the server's pick (x/acquire, stream); either
// file becomes an input of the core's player (/play/input/<id>). A
// series asks the season and episode first (?season=&episode= of a «Вышло
// новое» card preselect them). «Выбрать раздачу…» opens the release table.
import { ApiError, core, ui, useUser } from '@antresol/addon-sdk'
import { useEffect, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import { playPinned } from '../api'
import { useT } from '../i18n'
import { usePinned } from '../pinned/PinnedRelease'
import { releaseLink, ReleaseTable } from '../shared/ReleaseTable'
import { inputPath, useAcquire, type AcquireTarget } from '../shared/useAcquire'
import { FALLBACK_SEASONS, type Seasons } from './DownloadSheet'
import styles from './TitlePanels.module.css'

const { Select, Sheet } = ui

const range = (n: number) => Array.from({ length: n }, (_, i) => i + 1)

export interface WatchSheetProps {
  kind: 'series' | 'movie'
  tmdbId: number
  title: string
  poster?: string
  year?: number
  onClose: () => void
}

export function WatchSheet({ kind, tmdbId, title, poster, year, onClose }: WatchSheetProps) {
  const t = useT()
  const user = useUser()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const series = kind === 'series'
  const { state, run, reset } = useAcquire()
  const pinned = usePinned(kind, tmdbId)
  const [pinBusy, setPinBusy] = useState(false)
  const [pinError, setPinError] = useState('')
  const [seasons, setSeasons] = useState<Seasons | null>(series ? null : [])
  useEffect(() => {
    if (!series) return
    const ac = new AbortController()
    core.getTitle('series', tmdbId, ac.signal).then(
      (ti) => setSeasons((ti.seasons ?? []).length > 0 ? ti.seasons : FALLBACK_SEASONS),
      () => !ac.signal.aborted && setSeasons(FALLBACK_SEASONS),
    )
    return () => ac.abort()
  }, [series, tmdbId])
  const list = seasons && seasons.length > 0 ? seasons : FALLBACK_SEASONS
  const [season, setSeason] = useState(() => Number(params.get('season')) || 0)
  const [episode, setEpisode] = useState(() => Number(params.get('episode')) || 1)
  const current = list.some((s) => s.number === season) ? season : (list[0]?.number ?? 1)
  const episodes = range(list.find((s) => s.number === current)?.episodes ?? 1)
  const ep = episodes.includes(episode) ? episode : 1
  const [releases, setReleases] = useState(false)
  const [picked, setPicked] = useState<string | null>(null)

  const target: AcquireTarget = { tmdbId, kind, title, poster, ...(series ? { season: current, episode: ep } : {}) }
  const watch = () => {
    reset()
    setPinError('')
    if (!pinned.pin) return void run(target, 'stream')
    setPinBusy(true)
    playPinned(kind, tmdbId, series ? current : undefined, series ? ep : undefined).then(
      ({ inputId }) => navigate(inputPath(inputId), { state: { title, poster } }),
      (e: unknown) => {
        setPinBusy(false)
        setPinError(e instanceof ApiError ? e.message : t('app.failedTryAgain'))
      },
    )
  }
  // a movie starts once the pin is known (a pin, none, or no answer: the server's pick)
  const startedOnce = useRef(false)
  useEffect(() => {
    if (series || startedOnce.current || !pinned.settled) return
    startedOnce.current = true
    watch()
  })

  const busy = state.status === 'busy' || pinBusy
  const error = pinError || (state.status === 'error' ? state.message : '')
  if (releases) {
    return (
      <Sheet
        title={t('search.releasesTitle', { title: `${title}${series ? t('item.releasesSeason', { season: current }) : year ? ` (${year})` : ''}` })}
        message={series ? t('item.pickHintSeason', { episode: ep }) : t('search.pickHint.video')}
        role="dialog"
        size="large"
        dismissOnOutside
        onCancel={onClose}
      >
        {error && (
          <p className={styles.acquireError} role="alert">
            {error}
          </p>
        )}
        <ReleaseTable
          tmdbId={tmdbId}
          kind={kind}
          season={series ? current : undefined}
          episode={series ? ep : undefined}
          mode="stream"
          busy={picked ? { link: picked, mode: 'stream' } : null}
          onPick={(r) => {
            const link = releaseLink(r)
            setPicked(link)
            void run(target, 'stream', { releaseLink: link, sourceId: r.sourceId }).finally(() => setPicked(null))
          }}
          admin={Boolean(user?.isAdmin)}
        />
      </Sheet>
    )
  }
  return (
    <Sheet
      title={t('watch.now')}
      message={busy ? t('item.preparing') : undefined}
      role="dialog"
      busy={busy}
      onCancel={onClose}
      actions={[
        { label: t('common.cancel'), kind: 'cancel', run: onClose },
        { label: t('item.pickReleaseWatch'), run: () => (reset(), setReleases(true)) },
        ...(series ? [{ label: t('comp.action.watch'), kind: 'primary' as const, disabled: !seasons, run: watch }] : []),
      ]}
    >
      {series && (
        <div className={styles.episodePick}>
          <Select
            showLabel
            label={t('common.kind.season')}
            value={String(current)}
            options={list.map((s) => ({ value: String(s.number), label: String(s.number) }))}
            onChange={(v) => {
              setSeason(Number(v))
              setEpisode(1)
              reset()
            }}
          />
          <Select showLabel label={t('common.kind.episode')} value={String(ep)} options={episodes.map((n) => ({ value: String(n), label: String(n) }))} onChange={(v) => setEpisode(Number(v))} />
        </div>
      )}
      {error && (
        <p className={styles.acquireError} role="alert">
          {error}
        </p>
      )}
    </Sheet>
  )
}
