// «Докачивать новые» (a series) / «Скачать, когда выйдет» (a movie) on a
// title page: follow / stop following; turning it on may carry the quality
// its downloads prefer (resolution). The follow comes from the addon
// (GET x/follow/by-tmdb: the core's item and title answers carry none)
// and is read again when a check of it ends (follow-checked).
// The server checks a new follow by itself (no check call after adding).
// Stopping is reversible for UNDO_MS: the DELETE waits behind an
// «Отменить» toast (and goes out at once when the page is left).
import { ApiError, useEvents, useUser } from '@antresol/addon-sdk'
import { useCallback, useEffect, useRef, useState } from 'react'
import { follow, getFollowByTMDB, setFollowActive, unfollow } from '../api'
import { tr as t } from '../i18n'
import type { Follow, FollowChecked, FollowResolution } from '../types'
import { FOLLOW_CHECKED } from './events'

export const UNDO_MS = 5000

/** What a page knows of a follow. */
export type FollowRef = Pick<Follow, 'id' | 'active' | 'addedBy' | 'mine'>

const refOf = (f: Follow | null): FollowRef | null => (f ? { id: f.id, active: f.active, addedBy: f.addedBy, mine: f.mine } : null)

export interface FollowState {
  /** The follow is known (false while it loads, and when the server cannot follow: no button then). */
  available: boolean
  following: boolean
  busy: boolean
  error: string
  /** On → off (undoable), or off → on with `resolution` (absent: the follow keeps its own). */
  toggle: (resolution?: FollowResolution) => Promise<void>
  /** «Слежение отменено» is on screen; undo() keeps the follow. */
  undoable: boolean
  undo: () => void
  /**
   * Another profile added this follow (not the admin's to change here):
   * its name, or 'другой профиль'; null when this profile may change it.
   */
  followedBy: string | null
}

export function useFollow(kind: 'series' | 'movie', tmdbId: number): FollowState {
  const [ref, setRef] = useState<FollowRef | null>(null)
  const [available, setAvailable] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // the follow whose DELETE waits for the undo window
  const [pending, setPending] = useState<FollowRef | null>(null)
  const timer = useRef<number | undefined>(undefined)
  const pendingRef = useRef<FollowRef | null>(null)
  pendingRef.current = pending
  const refRef = useRef<FollowRef | null>(null)
  refRef.current = ref

  // the server's follow, unless a stop waits in the undo window (the server
  // still has the follow until then)
  const load = useCallback(
    (signal?: AbortSignal) =>
      getFollowByTMDB(kind, tmdbId, signal).then(
        (f) => {
          if (signal?.aborted) return
          if (!pendingRef.current) setRef(refOf(f))
          setAvailable(true)
        },
        () => undefined,
      ),
    [kind, tmdbId],
  )
  useEffect(() => {
    setRef(null)
    setAvailable(false)
    if (tmdbId <= 0) return
    const ac = new AbortController()
    void load(ac.signal)
    return () => ac.abort()
  }, [load, tmdbId])
  // a check of this follow ended (it may have been switched off or on elsewhere)
  useEvents(
    (e) => {
      const d = e.data as FollowChecked | null
      if (d && typeof d.followId === 'number' && d.followId === refRef.current?.id) void load()
    },
    [FOLLOW_CHECKED],
  )

  const commit = useCallback((f: FollowRef) => {
    window.clearTimeout(timer.current)
    setPending(null)
    unfollow(f.id).catch((e: unknown) => {
      setRef(f) // still followed on the server
      setError(e instanceof ApiError ? e.message : t('app.follow.unfollowFailed'))
    })
  }, [])
  // leaving the page within the window: stop following now
  useEffect(
    () => () => {
      window.clearTimeout(timer.current)
      if (pendingRef.current) void unfollow(pendingRef.current.id).catch(() => {})
    },
    [],
  )

  const user = useUser()
  // the server lets only the author and the admin change a follow (whose it is: by id, `mine`)
  const followedBy = ref && !user?.isAdmin && !ref.mine ? ref.addedBy || t('app.follow.anotherProfile') : null
  const following = Boolean(ref?.active)
  const toggle = async (resolution?: FollowResolution) => {
    setError('')
    if (followedBy) return
    if (following && ref) {
      setPending(ref)
      setRef(null)
      window.clearTimeout(timer.current)
      timer.current = window.setTimeout(() => commit(ref), UNDO_MS)
      return
    }
    if (pending) {
      // «Следить» again inside the window = undo
      undo()
      return
    }
    setBusy(true)
    try {
      if (ref) {
        // followed before and switched off in «Слежение»: switch it back on
        setRef(refOf(await setFollowActive(ref.id, true, resolution)))
      } else {
        const f = await follow(kind, tmdbId, resolution)
        // an existing follow comes back as is: another profile's stays theirs
        const theirs = !user?.isAdmin && !f.mine
        setRef(refOf(f.active || theirs ? f : await setFollowActive(f.id, true, resolution)))
      }
    } catch (e) {
      setError(e instanceof ApiError ? e.message : t('app.failedTryAgain'))
    } finally {
      setBusy(false)
    }
  }
  const undo = () => {
    const f = pendingRef.current
    if (!f) return
    window.clearTimeout(timer.current)
    setPending(null)
    setRef(f)
  }
  return { available, following, busy, error, toggle, undoable: pending !== null, undo, followedBy }
}
