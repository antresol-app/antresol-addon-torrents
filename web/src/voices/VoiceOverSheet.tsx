// «Найти озвучку…» (adult profiles): the voice-over releases of
// a film or series, one search of the sources per opening. A row: the
// studio, the release, what it covers of the server's episodes, its size
// and seeders, «Скачать». A download shows its progress in the row, then
// each episode's fit («подогнано +1,2 с» / «не подходит: другая версия»).
// Video releases naming a studio are listed apart: their file list is
// fetched only when the user asks («Проверить»), and only their audio
// files download. The addon's item action (a library video's page) and
// player track action (the audio menu) return it as their sheet.
import { ApiError, formatPercent, formatSize, Icon, locale, ui } from '@antresol/addon-sdk'
import { useCallback, useEffect, useRef, useState } from 'react'
import { downloadVoiceOver, findVoiceOvers, previewVoiceOver, voiceOverDownloads } from '../api'
import { tr, useT } from '../i18n'
import type { VoiceOverCandidate, VoiceOverCoverage, VoiceOverDownload, VoiceOverEpisode, VoiceOvers } from '../types'
import styles from './VoiceOverSheet.module.css'

const { Button, Sheet, Spinner } = ui

export interface VoiceOverSheetProps {
  /** The film or series (library item id). */
  itemId: number
  title: string
  onClose: () => void
  /** Mount point (the player's root, so it shows in full screen). */
  container?: Element | null
  /** How often a running download is looked at (ms). */
  pollMs?: number
}

type State = { status: 'loading' } | { status: 'error'; message: string } | { status: 'ready'; data: VoiceOvers }

const errText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback)

export function coverageLabel(c: VoiceOverCoverage): string {
  if (c.film) return tr('vo.film')
  if (!c.known) return tr('vo.coverageUnknown')
  return tr('vo.coverage', { have: c.have, count: c.total })
}

/** «+1,2 с» / "+1.2 s": an offset in seconds with its sign. */
function offsetLabel(ms: number): string {
  const s = (ms / 1000).toLocaleString(locale(), { minimumFractionDigits: 1, maximumFractionDigits: 1, signDisplay: 'always' })
  return tr('vo.seconds', { n: s })
}

export function episodeState(e: VoiceOverEpisode): string {
  switch (e.state) {
    case 'fitted': {
      const offset = offsetLabel(e.offsetMs ?? 0)
      return e.tempo && e.tempo !== 1 ? tr('vo.state.fittedPal', { offset }) : tr('vo.state.fitted', { offset })
    }
    case 'placed':
      return tr('vo.state.placed')
    case 'unmeasured':
      return tr('vo.state.unmeasured')
    case 'misfit':
      return tr('vo.state.misfit')
    case 'skipped':
      return tr('vo.state.skipped')
    case 'had':
      return tr('vo.state.had')
    default:
      return tr('vo.state.downloading')
  }
}

const episodeName = (e: VoiceOverEpisode) => (e.episode ? `S${e.season} E${e.episode}` : tr('vo.filmEpisode'))

/** A download still has work: its torrent, or a track waiting for its fit. */
const running = (d: VoiceOverDownload) => !d.done || d.episodes.some((e) => !e.state || e.state === 'downloading' || e.state === 'placed')

export function VoiceOverSheet({ itemId, title, onClose, container, pollMs = 3000 }: VoiceOverSheetProps) {
  const t = useT()
  const [state, setState] = useState<State>({ status: 'loading' })
  const [downloads, setDownloads] = useState<VoiceOverDownload[]>([])
  // candidate id → its download's infohash (started here, or matched by name)
  const [started, setStarted] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState<string | null>(null)
  const [rowError, setRowError] = useState<Record<string, string>>({})
  const [previews, setPreviews] = useState<Record<string, { coverage: VoiceOverCoverage; message?: string }>>({})
  const alive = useRef(true)
  useEffect(() => () => void (alive.current = false), [])

  const search = useCallback(() => {
    const ac = new AbortController()
    setState({ status: 'loading' })
    Promise.all([findVoiceOvers(itemId, ac.signal), voiceOverDownloads(itemId, ac.signal).catch(() => [] as VoiceOverDownload[])]).then(
      ([data, ds]) => {
        setState({ status: 'ready', data })
        setDownloads(ds)
        // a release downloaded before: its row shows that download
        const byTitle: Record<string, string> = {}
        for (const c of [...data.candidates, ...data.videoReleases]) {
          const d = ds.find((x) => x.title === c.title)
          if (d) byTitle[c.id] = d.infoHash
        }
        setStarted(byTitle)
      },
      (e: unknown) => {
        if (!ac.signal.aborted) setState({ status: 'error', message: errText(e, tr('vo.failed')) })
      },
    )
    return () => ac.abort()
  }, [itemId])
  useEffect(() => search(), [search])

  /** The downloads as the server lists them now: a started one it no longer lists (removed in «Загрузки») is no longer shown or watched. */
  const listed = useCallback((ds: VoiceOverDownload[]) => {
    setDownloads(ds)
    setFresh(new Set())
    setStarted((s) => {
      const kept = Object.fromEntries(Object.entries(s).filter(([, ih]) => ds.some((d) => d.infoHash === ih)))
      return Object.keys(kept).length === Object.keys(s).length ? s : kept
    })
  }, [])
  // started here and not yet seen in a listing (the one right after
  // «Скачать» failed): looked at until a listing says
  const [fresh, setFresh] = useState<Set<string>>(new Set())

  // a running download is looked at again until its tracks are fitted
  const watching = Object.values(started).some((ih) => {
    const d = downloads.find((x) => x.infoHash === ih)
    return fresh.has(ih) || Boolean(d && running(d))
  })
  useEffect(() => {
    if (!watching) return
    const id = window.setInterval(() => {
      voiceOverDownloads(itemId).then((ds) => alive.current && listed(ds), () => {})
    }, pollMs)
    return () => window.clearInterval(id)
  }, [watching, itemId, pollMs, listed])

  const download = async (c: VoiceOverCandidate) => {
    setBusy(c.id)
    setRowError((r) => ({ ...r, [c.id]: '' }))
    try {
      const res = await downloadVoiceOver(itemId, c.id)
      if (!alive.current) return
      setStarted((s) => ({ ...s, [c.id]: res.infoHash }))
      const ds = await voiceOverDownloads(itemId).catch(() => null)
      if (!alive.current) return
      if (ds) setDownloads(ds) // the next poll prunes one removed meanwhile
      else setFresh((f) => new Set(f).add(res.infoHash)) // asked again by the poll
    } catch (e) {
      if (alive.current) setRowError((r) => ({ ...r, [c.id]: errText(e, tr('common.failed')) }))
    } finally {
      if (alive.current) setBusy(null)
    }
  }

  const check = async (c: VoiceOverCandidate) => {
    setBusy(c.id)
    setRowError((r) => ({ ...r, [c.id]: '' }))
    try {
      const p = await previewVoiceOver(itemId, c.id)
      if (alive.current) setPreviews((s) => ({ ...s, [c.id]: { coverage: p.coverage, message: p.message } }))
    } catch (e) {
      if (alive.current) setRowError((r) => ({ ...r, [c.id]: errText(e, tr('common.failed')) }))
    } finally {
      if (alive.current) setBusy(null)
    }
  }

  const row = (c: VoiceOverCandidate) => {
    const studio = c.studio || t('vo.noStudio')
    const preview = previews[c.id]
    const coverage = preview?.coverage ?? c.coverage
    const ih = started[c.id]
    const d = ih ? downloads.find((x) => x.infoHash === ih) : undefined
    // a video release: «Скачать» once its file list showed separate tracks
    const canDownload = !c.video || (preview && !preview.message && preview.coverage.have > 0)
    return (
      <li key={c.id} className={styles.row}>
        <div className={styles.line}>
          <div className={styles.text}>
            <p className={styles.studio}>{studio}</p>
            <p className={styles.release} title={c.title}>
              {c.title}
            </p>
            <p className={styles.meta}>
              {/* a video release: what it covers is known once its files are */}
              {(!c.video || preview) && <span>{coverageLabel(coverage)}</span>}
              {c.size > 0 && <span>{formatSize(c.size)}</span>}
              <span>{t('vo.seeders', { n: c.seeders.toLocaleString(locale()) })}</span>
            </p>
          </div>
          {!ih &&
            (canDownload ? (
              <Button
                size="small"
                variant="primary"
                icon="download"
                aria-label={t('vo.downloadAria', { studio })}
                loading={busy === c.id}
                disabled={busy !== null}
                onClick={() => void download(c)}
              >
                {t('vo.download')}
              </Button>
            ) : (
              !preview && (
                <Button
                  size="small"
                  variant="secondary"
                  aria-label={t('vo.checkAria', { studio })}
                  loading={busy === c.id}
                  disabled={busy !== null}
                  onClick={() => void check(c)}
                >
                  {t('vo.check')}
                </Button>
              )
            ))}
        </div>
        {preview?.message && <p className={styles.note}>{preview.message}</p>}
        {rowError[c.id] && (
          <p className={styles.problem} role="alert">
            {rowError[c.id]}
          </p>
        )}
        {ih && <DownloadState studio={studio} download={d} />}
      </li>
    )
  }

  return (
    <Sheet title={t('vo.title')} message={title} role="dialog" size="large" dismissOnOutside container={container} onCancel={onClose}>
      {state.status === 'loading' && (
        <p className={styles.state} role="status">
          <Spinner track className={styles.spinner} /> {t('vo.searching')}
        </p>
      )}
      {state.status === 'error' && (
        <div className={styles.empty}>
          <Icon name="cloud" size={36} strokeWidth={1.5} />
          <p role="alert">{state.message}</p>
          <Button size="small" variant="secondary" onClick={search}>
            {t('common.retry')}
          </Button>
        </div>
      )}
      {state.status === 'ready' && (
        <>
          {state.data.candidates.length === 0 && state.data.videoReleases.length === 0 && (
            <div className={styles.empty} role="status">
              <Icon name="speaker" size={36} strokeWidth={1.5} />
              <p className={styles.emptyTitle}>{t('vo.none')}</p>
              <p>{t('vo.noneHint')}</p>
            </div>
          )}
          {state.data.candidates.length > 0 && (
            <ul className={styles.list} aria-label={t('vo.results')}>
              {state.data.candidates.map(row)}
            </ul>
          )}
          {state.data.videoReleases.length > 0 && (
            <section className={styles.section}>
              <h3 className={styles.heading}>{t('vo.videoHeading')}</h3>
              <p className={styles.hint}>{t('vo.videoHint')}</p>
              <ul className={styles.list} aria-label={t('vo.videoList')}>
                {state.data.videoReleases.map(row)}
              </ul>
            </section>
          )}
          {state.data.failed.length > 0 && (
            <p className={styles.hint}>{t('vo.notAnswered', { names: state.data.failed.map((f) => f.name).join(', ') })}</p>
          )}
        </>
      )}
    </Sheet>
  )
}

/** A started download in its row: the progress, then each episode's fit. */
function DownloadState({ studio, download: d }: { studio: string; download?: VoiceOverDownload }) {
  const t = useT()
  const pct = Math.round((d?.progress ?? 0) * 100)
  if (!d || !d.done) {
    return (
      <div className={styles.progress}>
        <div
          className={styles.bar}
          role="progressbar"
          aria-label={t('vo.progressAria', { studio })}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
        >
          <span style={{ width: `${pct}%` }} />
        </div>
        <span className={styles.meta}>{t('vo.progress', { pct: formatPercent(pct) })}</span>
      </div>
    )
  }
  const fitting = d.episodes.some((e) => e.state === 'placed')
  return (
    <div className={styles.result}>
      <p className={styles.meta} role="status">
        {fitting ? (
          <>
            <Spinner track className={styles.spinner} /> {t('vo.fitting')}
          </>
        ) : (
          <>
            <Icon name="checkCircle" size={14} strokeWidth={2.2} /> {t('vo.ready')}
          </>
        )}
      </p>
      <ul className={styles.episodes}>
        {d.episodes.map((e) => (
          <li key={e.fileId} className={e.state === 'misfit' || e.state === 'skipped' || e.state === 'unmeasured' ? styles.bad : e.state === 'fitted' ? styles.good : undefined}>
            <span className={styles.code}>{episodeName(e)}</span>
            <span>{episodeState(e)}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

/** «Найти озвучку» on an item's page: a film or series of the library. */
export const libraryVideo = (it: { id: number; kind: string }) => it.id > 0 && (it.kind === 'movie' || it.kind === 'series')

/** The item action's run: the sheet over the item's page. */
export const openVoiceOverSheet = (it: { id: number; title: string }, close: () => void) => <VoiceOverSheet itemId={it.id} title={it.title} onClose={close} />
