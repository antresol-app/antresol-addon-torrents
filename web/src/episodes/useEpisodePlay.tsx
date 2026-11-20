// The addon's player of the episode guide (episodeActions, kinds pinned and
// torrent; the core plays library and online sources itself): a pinned
// source streams from the household's pin, a torrent source streams that
// release; either becomes an input of the core's player (/play/input/<id>).
// «Другие источники…» opens the release table on that episode (when
// the core has no online picker for it); «Скачать серию» downloads one
// episode of the pin to the server.
import { ApiError, ui, useUser, useWatchWay, type EpisodeCtx, type EpisodePlay, type EpisodeSource, type TitleEpisode } from '@antresol/addon-sdk'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { downloadPinnedEpisodes, ID, playPinned } from '../api'
import { useT } from '../i18n'
import { releaseLink, ReleaseTable } from '../shared/ReleaseTable'
import { inputPath, useAcquire, type AcquireTarget } from '../shared/useAcquire'
import styles from '../titlepage/TitlePanels.module.css'

const { Sheet, Toast } = ui

/** How long a note stays on screen. */
const NOTE_MS = 5000

/** The way id of the addon's «Смотреть сейчас» in the core's «Смотреть ▾» (`<addonId>:<label>`). */
export const WATCH_WAY = `${ID}:watch.now`
/** The kinds a tap prefers once the profile chose «Смотреть сейчас» last (the pin covers the release). */
const PREFER = ['pinned', 'torrent'] as const

type At = { season: number; episode: number }

export function useEpisodePlay(ctx: EpisodeCtx): EpisodePlay | null {
  const { kind, tmdbId, title, poster } = ctx
  const t = useT()
  const navigate = useNavigate()
  const user = useUser()
  const { state, run, reset, track, cancel } = useAcquire()
  const [releases, setReleases] = useState<At | null>(null)
  const [picked, setPicked] = useState<string | null>(null)
  const [note, setNote] = useState<{ text: string; error?: boolean; key: number } | null>(null)
  const say = (text: string, error = false) => setNote({ text, error, key: Date.now() })
  useEffect(() => {
    if (!note) return
    const id = window.setTimeout(() => setNote(null), NOTE_MS)
    return () => window.clearTimeout(id)
  }, [note])
  // a stream that failed outside the release sheet: a note, not a lasting error
  const streamError = state.status === 'error' && !releases ? state.message : ''
  useEffect(() => {
    if (!streamError) return
    say(streamError, true)
    reset()
  }, [streamError])

  // the latest render's values for the stable answer's functions
  const live = useRef({ ctx, run, reset, t, navigate, track })
  live.current = { ctx, run, reset, t, navigate, track }
  const target = (at: At): AcquireTarget => {
    const c = live.current.ctx
    return { tmdbId: c.tmdbId, kind: c.kind, title: c.title, poster: c.poster, ...(c.kind === 'series' ? { season: at.season, episode: at.episode } : {}) }
  }
  const failed = (e: unknown) => say(e instanceof ApiError ? e.message : live.current.t('app.failedTryAgain'), true)

  const busy = state.status === 'busy'
  const message = busy ? t('item.preparing') : note?.text
  // the profile's remembered «Смотреть ▾» way (the SDK's, live)
  const way = useWatchWay()
  const prefer = user && way === WATCH_WAY ? PREFER : undefined
  const node = (
    <>
      {releases && (
        <Sheet
          title={t('search.releasesTitle', { title: `${title}${kind === 'series' ? t('item.releasesSeason', { season: releases.season }) : ''}` })}
          message={kind === 'series' ? t('item.pickHintSeason', { episode: releases.episode }) : t('search.pickHint.video')}
          role="dialog"
          size="large"
          dismissOnOutside
          onCancel={() => (setReleases(null), cancel())}
        >
          {state.status === 'error' && (
            <p className={styles.acquireError} role="alert">
              {state.message}
            </p>
          )}
          <ReleaseTable
            tmdbId={tmdbId}
            kind={kind}
            season={kind === 'series' ? releases.season : undefined}
            episode={kind === 'series' ? releases.episode : undefined}
            mode="stream"
            busy={picked ? { link: picked, mode: 'stream' } : null}
            onPick={(r) => {
              const link = releaseLink(r)
              setPicked(link)
              void run(target(releases), 'stream', { releaseLink: link, sourceId: r.sourceId }).finally(() => setPicked(null))
            }}
            admin={Boolean(user?.isAdmin)}
          />
        </Sheet>
      )}
      <Toast tone={!busy && note?.error ? 'error' : 'default'} messageKey={busy ? 'busy' : note?.key}>
        {message}
      </Toast>
    </>
  )

  // one answer per state the guide needs (the core re-runs on a new one)
  const answer = useMemo<EpisodePlay>(
    () => ({
      play(ep: TitleEpisode, src: EpisodeSource) {
        const { ctx: c, run: go, navigate: nav, track: begin } = live.current
        const at = { season: ep.season, episode: ep.number }
        if (src.kind === 'pinned') {
          // aborted by another start or the page going away: no player opens
          const signal = begin()
          playPinned(c.kind, c.tmdbId, c.kind === 'series' ? at.season : undefined, c.kind === 'series' ? at.episode : undefined, signal).then(
            ({ inputId }) => !signal.aborted && nav(inputPath(inputId), { state: { title: c.title, poster: c.poster } }),
            (e: unknown) => !signal.aborted && failed(e),
          )
          return
        }
        if (src.kind === 'torrent') void go(target(at), 'stream', { releaseLink: src.ref, sourceId: Number(src.meta.sourceId) || undefined })
      },
      other(ep: TitleEpisode) {
        live.current.reset()
        setReleases({ season: ep.season, episode: ep.number })
      },
      download(ep: TitleEpisode, src: EpisodeSource) {
        if (src.kind !== 'pinned') return
        const c = live.current.ctx
        downloadPinnedEpisodes(c.kind, c.tmdbId, ep.season, [ep.number]).then(
          (r) => say(r.note || live.current.t('episodes.download.started', { n: ep.number })),
          failed,
        )
      },
      prefer,
      node,
    }),
    // node changes with what it shows; the functions read the latest values
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [prefer, releases, picked, state, note, title, kind, tmdbId, user?.isAdmin],
  )
  // a kid plays only the library (the core gives a kid no addon entries anyway)
  if (!user || user.isKid) return null
  return answer
}
