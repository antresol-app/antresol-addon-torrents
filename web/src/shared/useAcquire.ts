// «Смотреть сейчас» and «На сервер» for a TMDB title, «На сервер» for an
// album, book, audiobook or manga (no «Смотреть сейчас»: they are only
// downloaded): one request each; the server picks the release unless the
// admin chose one in the release table.
import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { ApiError } from '@antresol/addon-sdk'
import { acquire, watchNow } from '../api'
import type { AcquireRequest, AcquireResult } from '../types'
import { tr as t } from '../i18n'

export interface AcquireTarget {
  /** 0 for music and books. */
  tmdbId: number
  kind: string
  title: string
  /** Music and books: the catalog id, the artist or author, the year (the album's folder). */
  extId?: string
  artist?: string
  year?: number
  /** Music: other names of the album (Candidate.aliases). */
  aliases?: string[]
  /** Music: «Скачать в другом формате» — the server may pick a release of another format. */
  anyFormat?: boolean
  poster?: string
  backdrop?: string
  season?: number
  episode?: number
}

export interface ReleaseChoice {
  releaseLink: string
  sourceId?: number
  /** The release is a file of an OPDS catalog (Release.direct). */
  direct?: boolean
  /** The release's name (music: kept for the organize check). */
  releaseTitle?: string
  /** Music: the file indexes chosen in the track step (absent = the whole torrent). */
  files?: number[]
  /** «Выбрать другую раздачу»: replaces the title's unfinished download. */
  replace?: boolean
  /** Movies and series, no release chosen: the quality the server's pick should have. */
  quality?: string
}

/** Kinds that are only downloaded to the server (no TMDB, no «Смотреть сейчас»). */
export const isMediaKind = (kind: string) => kind === 'music' || kind === 'book' || kind === 'audiobook' || kind === 'comic'

/** The request of a target: a TMDB title, or an album / book by its catalog data. */
export function acquireRequest(t: AcquireTarget, mode: 'stream' | 'download', choice?: ReleaseChoice): AcquireRequest {
  const pick = choice
    ? { releaseLink: choice.releaseLink || undefined, sourceId: choice.sourceId, direct: choice.direct || undefined, releaseTitle: (t.kind === 'music' && choice.releaseTitle) || undefined, files: choice.files, replace: choice.replace || undefined, quality: choice.quality || undefined }
    : {}
  if (isMediaKind(t.kind)) {
    return { tmdbId: 0, kind: t.kind, mode: 'download', extId: t.extId || undefined, title: t.title, artist: t.artist || undefined, year: t.year || undefined, aliases: t.aliases?.length ? t.aliases : undefined, poster: t.poster || undefined, anyFormat: t.anyFormat || undefined, ...pick }
  }
  return { tmdbId: t.tmdbId, kind: t.kind, mode, season: t.season, episode: t.episode, ...pick }
}

/** «… загружается на сервер» in the words of the kind. */
function downloadingMessage(tg: AcquireTarget): string {
  switch (tg.kind) {
    case 'music':
      return t('app.acquire.downloadingAlbum', { title: tg.title })
    case 'book':
      return t('app.acquire.downloadingBook', { title: tg.title })
    case 'audiobook':
      return t('app.acquire.downloadingAudiobook', { title: tg.title })
    case 'comic':
      return t('app.acquire.downloadingComic', { title: tg.title })
  }
  return tg.kind === 'series' && tg.season
    ? t('app.acquire.downloadingSeason', { season: tg.season, title: tg.title })
    : t('app.acquire.downloadingTitle', { title: tg.title })
}

export type AcquireState =
  | { status: 'idle' }
  | { status: 'busy'; mode: 'stream' | 'download' }
  | { status: 'done'; message: string }
  /** otherFormat: no release in «Формат загрузки» — «Скачать в другом формате» starts one of another format. */
  | { status: 'error'; message: string; otherFormat?: () => void }

/** The core's player of an addon input (/play/input/<id>); the title and art travel in the URL state. */
export const inputPath = (inputId: string) => `/play/input/${encodeURIComponent(inputId)}`

export function useAcquire() {
  const navigate = useNavigate()
  const [state, setState] = useState<AcquireState>({ status: 'idle' })
  // the running «Смотреть сейчас» (a stream or a pinned play): Cancel, a
  // new one, or the caller going away aborts it — the requests stop and the
  // player never opens. A download is not aborted (it goes on on the server).
  const playing = useRef<AbortController | null>(null)
  useEffect(() => () => playing.current?.abort(), [])

  /** A new «Смотреть сейчас»'s signal (the one before it is aborted). */
  const track = useCallback((): AbortSignal => {
    playing.current?.abort()
    const ac = new AbortController()
    playing.current = ac
    return ac.signal
  }, [])
  /** «Отменить»: the running «Смотреть сейчас» stops; the state is idle again. */
  const cancel = useCallback(() => {
    playing.current?.abort()
    playing.current = null
    setState({ status: 'idle' })
  }, [])

  /**
   * Resolves true once the server took the request (the download button's
   * ring starts then). `send` asks another endpoint than /api/acquire for
   * the same answer (a stream from the title's pinned release). A cancelled
   * stream resolves false and opens nothing.
   */
  const run = useCallback(
    async (
      tg: AcquireTarget,
      mode: 'stream' | 'download',
      choice?: ReleaseChoice,
      send: (req: AcquireRequest, signal?: AbortSignal) => Promise<AcquireResult> = acquire,
    ): Promise<boolean> => {
      if (isMediaKind(tg.kind)) mode = 'download'
      const signal = mode === 'stream' ? track() : undefined
      setState({ status: 'busy', mode })
      const req = acquireRequest(tg, mode, choice)
      try {
        const r = await send(req, signal)
        if (signal?.aborted) return false
        if (mode === 'stream') {
          // «Смотреть сейчас»: the file becomes an input of the core's player
          const { inputId } = await watchNow(r.infoHash, r.fileIndex, signal)
          if (signal?.aborted) return false
          setState({ status: 'idle' })
          navigate(inputPath(inputId), { state: { title: tg.title, poster: tg.poster } })
          return true
        }
        setState({
          status: 'done',
          message: [r.status === 'waiting_space' ? t('app.acquire.waitingSpace') : downloadingMessage(tg), r.note].filter(Boolean).join(' '),
        })
        return true
      } catch (e) {
        if (signal?.aborted) return false
        const otherFormat = e instanceof ApiError && e.code === 'no_format' && !choice ? () => void run({ ...tg, anyFormat: true }, mode) : undefined
        setState({ status: 'error', message: e instanceof ApiError ? e.message : t('app.failedTryAgain'), otherFormat })
        return false
      }
    },
    [navigate, track],
  )

  /** «Все сезоны на сервер»: one download per season, one after another. */
  const runSeasons = useCallback(async (tg: AcquireTarget, seasons: number[], quality?: string): Promise<boolean> => {
    setState({ status: 'busy', mode: 'download' })
    const failed: string[] = []
    let waiting = false
    for (const n of seasons) {
      try {
        const r = await acquire({ tmdbId: tg.tmdbId, kind: tg.kind, mode: 'download', season: n, quality: quality || undefined })
        waiting ||= r.status === 'waiting_space'
      } catch (e) {
        failed.push(t('app.acquire.seasonFailed', { season: n, reason: e instanceof ApiError ? e.message : t('app.acquire.failedLower') }))
      }
    }
    const ok = seasons.length - failed.length
    if (ok === 0) {
      setState({ status: 'error', message: t('app.acquire.noSeasons', { reasons: failed.join('; ') }) })
      return false
    }
    let message = t('app.acquire.seasonsAdded', { ok, total: seasons.length, title: tg.title })
    if (waiting) message += t('app.acquire.someWaiting')
    if (failed.length) message += t('app.acquire.someFailed', { reasons: failed.join('; ') })
    setState({ status: 'done', message })
    return true
  }, [])

  return { state, run, runSeasons, track, cancel, reset: () => setState({ status: 'idle' }) }
}
