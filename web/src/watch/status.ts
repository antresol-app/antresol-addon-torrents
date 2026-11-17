// The status of a «Смотреть сейчас» download (x/torrents/{ih}/status?file=),
// polled STATUS_POLL_MS after each answer (never two at once) for as long
// as anything on screen reads it, until the download is complete or gone:
// the player's note, its keep action and its progress hook share one poll
// per file.
import { ApiError, type PlayerInput } from '@antresol/addon-sdk'
import { useCallback, useSyncExternalStore } from 'react'
import { getTorrentStatus } from '../api'
import type { StreamStatus } from '../types'

export type { StreamStatus }

export const STATUS_POLL_MS = 2000

/** What x/watch put in the input's meta: the download and its file (null: not one of ours). */
export function watchOf(input: PlayerInput): { ih: string; idx: number; mode: string } | null {
  const m = input.meta as { ih?: unknown; idx?: unknown; mode?: unknown } | null | undefined
  if (!m || typeof m.ih !== 'string' || !m.ih || typeof m.idx !== 'number') return null
  return { ih: m.ih, idx: m.idx, mode: typeof m.mode === 'string' ? m.mode : '' }
}

interface Poll {
  status: StreamStatus | null
  readers: Set<() => void>
  stop: () => void
}
const polls = new Map<string, Poll>()

/** Nothing more to learn: every byte is there. */
const complete = (s: StreamStatus) => s.size > 0 && s.done >= s.size

function start(key: string, ih: string, idx: number): Poll {
  const ac = new AbortController()
  const p: Poll = { status: null, readers: new Set(), stop: () => {} }
  let timer: number | undefined
  // the next poll is set after each answer: a slow core never stacks them
  const next = () => {
    if (!ac.signal.aborted) timer = window.setTimeout(tick, STATUS_POLL_MS)
  }
  const tick = () => {
    getTorrentStatus(ih, ac.signal, idx).then(
      (s) => {
        if (ac.signal.aborted) return
        p.status = s
        for (const r of p.readers) r()
        if (!complete(s)) next() // complete: what is shown stays, nothing to ask
      },
      (e: unknown) => {
        // gone (404): nothing to poll; another failure keeps what was shown and asks again
        if (!(e instanceof ApiError && e.status === 404)) next()
      },
    )
  }
  tick()
  p.stop = () => {
    window.clearTimeout(timer)
    ac.abort()
  }
  return p
}

/** The download's status (null: none yet, or `w` is null); polled while mounted. */
export function useWatchStatus(w: { ih: string; idx: number } | null): StreamStatus | null {
  const key = w ? `${w.ih}/${w.idx}` : ''
  const subscribe = useCallback(
    (onChange: () => void) => {
      if (!w) return () => {}
      let p = polls.get(key)
      if (!p) polls.set(key, (p = start(key, w.ih, w.idx)))
      p.readers.add(onChange)
      return () => {
        p.readers.delete(onChange)
        if (p.readers.size === 0) {
          p.stop()
          polls.delete(key)
        }
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  )
  return useSyncExternalStore(subscribe, () => polls.get(key)?.status ?? null, () => null)
}
