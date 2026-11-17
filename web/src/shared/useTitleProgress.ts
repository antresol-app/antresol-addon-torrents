// Progress of one title's download, for the round download button's ring.
// Asks the server on mount and when the title changes; then it follows the
// adults' `progress` event (addon.torrents.progress: every 2 s while a
// library download runs). It polls every POLL_MS while the torrent runs
// only when no event came for EVENT_FRESH_MS (no stream, an old server).
// An admin also gets addon.torrents.admin.progress: another list, not
// listened to here.
import { useEvents, type DownloadState } from '@antresol/addon-sdk'
import { useCallback, useEffect, useRef, useState } from 'react'
import { getTitleProgressBatched } from '../api'
import type { TitleProgress, TorrentHint } from '../types'

/** An entry of the `progress` event: a library download of a title. */
interface LibraryProgress {
  key: string
  kind: string
  tmdbId: number
  season: number
  status: 'active' | 'waiting' | 'paused' | 'organizing' | 'error' | (string & {})
  done: number
  size: number
  /** Why it makes no headway, in words (both languages); the ring asks the server for the details. */
  hint?: { ru: string; en: string }
}

export interface TitleProgressQuery {
  kind: string
  tmdbId: number
  season?: number
}

export const POLL_MS = 2000
/** After a start the torrent may not be registered yet: «starting» holds this long over empty answers. */
export const STARTING_GRACE_MS = 10_000
/** An event this recent stands for a poll. */
export const EVENT_FRESH_MS = 10_000
export const PROGRESS_EVENT = 'addon.torrents.progress'
const EVENTS = [PROGRESS_EVENT]

const IDLE: DownloadState<TorrentHint> = { kind: 'idle' }
const keyOf = (q: TitleProgressQuery | null) => (q ? `${q.kind}|${q.tmdbId}|${q.season ?? ''}` : '')

export function useTitleProgress(q: TitleProgressQuery | null): { state: DownloadState<TorrentHint>; watch: () => void; refresh: () => void } {
  const key = keyOf(q)
  // the state is stored with its title: another title's state is never shown
  const [snap, setSnap] = useState<{ key: string; state: DownloadState<TorrentHint> }>({ key, state: IDLE })
  const [kick, setKick] = useState(0)
  const snapRef = useRef(snap)
  snapRef.current = snap
  const qRef = useRef(q)
  qRef.current = q
  const grace = useRef({ key: '', until: 0 })
  // when the last `progress` event came (0: none yet)
  const lastEvent = useRef(0)
  const fresh = () => lastEvent.current > 0 && Date.now() - lastEvent.current < EVENT_FRESH_MS

  useEffect(() => {
    setSnap((s) => (s.key === key ? s : { key, state: IDLE }))
    const query = qRef.current
    if (!query) return
    const ac = new AbortController()
    let timer: number | undefined
    let polling = grace.current.key === key && Date.now() < grace.current.until
    const set = (state: DownloadState<TorrentHint>) => setSnap({ key, state })

    const tick = async () => {
      // the event keeps the ring up to date: look again later
      if (polling && fresh()) {
        timer = window.setTimeout(tick, POLL_MS)
        return
      }
      let p
      try {
        p = await getTitleProgressBatched(query, ac.signal)
      } catch {
        // a failed poll keeps what the ring shows; a running download is asked again
        if (!ac.signal.aborted && polling) timer = window.setTimeout(tick, POLL_MS)
        return
      }
      if (ac.signal.aborted) return
      // an answer without a known status (an old server's {}) is «nothing downloading»
      if (p === null || (p.status !== 'active' && p.status !== 'paused' && p.status !== 'done')) {
        if (grace.current.key === key && Date.now() < grace.current.until) {
          set({ kind: 'starting' })
          timer = window.setTimeout(tick, POLL_MS)
        } else {
          polling = false
          set(IDLE)
        }
        return
      }
      if (p.status === 'done') {
        polling = false
        set({ kind: 'done' })
        return
      }
      polling = true
      set(p.status === 'active' ? { kind: 'active', done: p.done, size: p.size, stalled: p.stalled || undefined, hint: hintOf(p) } : { kind: 'paused', done: p.done, size: p.size })
      timer = window.setTimeout(tick, POLL_MS)
    }
    void tick()
    return () => {
      ac.abort()
      window.clearTimeout(timer)
    }
  }, [key, kick])

  useEvents((e) => {
    const query = qRef.current
    if (e.type !== PROGRESS_EVENT || !query || !Array.isArray(e.data)) return
    lastEvent.current = Date.now()
    const k = keyOf(query)
    const mine = (e.data as LibraryProgress[]).find((p) => p.kind === query.kind && p.tmdbId === query.tmdbId && p.season === (query.season ?? 0))
    const prev = snapRef.current.key === k ? snapRef.current.state : IDLE
    const next = mine ? fromEvent(mine, prev) : undefined
    if (next) {
      setSnap({ key: k, state: next })
      // a stall the event names in words only: the server has its kind
      if (mine?.hint && !(next.kind === 'active' && (next.stalled || next.hint))) setKick((n) => n + 1)
      return
    }
    // not listed (done, cancelled, or another season's), or a status the ring
    // has no word for: ask the server once, unless a start is still settling
    const settling = grace.current.key === k && Date.now() < grace.current.until
    if (prev.kind !== 'idle' && prev.kind !== 'done' && !settling) setKick((n) => n + 1)
  }, EVENTS)

  // call right after a download was started: «starting» now, polling at once
  const watch = useCallback(() => {
    if (!qRef.current) return
    const k = keyOf(qRef.current)
    grace.current = { key: k, until: Date.now() + STARTING_GRACE_MS }
    setSnap({ key: k, state: { kind: 'starting' } })
    setKick((n) => n + 1)
  }, [])

  // call after a download was cancelled: ask again, without the start's grace
  const refresh = useCallback(() => {
    grace.current = { key: '', until: 0 }
    setKick((n) => n + 1)
  }, [])

  return { state: snap.key === key ? snap.state : IDLE, watch, refresh }
}

/**
 * The ring's state from an event entry (undefined: ask the server). A stall
 * the event still names keeps the kind and hint the server gave last.
 */
function fromEvent(p: LibraryProgress, prev: DownloadState<TorrentHint>): DownloadState<TorrentHint> | undefined {
  switch (p.status) {
    case 'active':
    case 'waiting': {
      const keep = p.hint && prev.kind === 'active' ? { stalled: prev.stalled, hint: prev.hint } : { stalled: undefined, hint: undefined }
      return { kind: 'active', done: p.done, size: p.size, ...keep }
    }
    case 'paused':
      return { kind: 'paused', done: p.done, size: p.size }
    case 'organizing':
      return { kind: 'done' }
  }
  return undefined
}

/** The liveness and tracker words of a title's progress (undefined: nothing to say). */
function hintOf(p: TitleProgress): TorrentHint | undefined {
  if (!p.liveness && !p.trackerErrorKind) return undefined
  return { liveness: p.liveness, trackerError: p.trackerError, trackerErrorKind: p.trackerErrorKind }
}
