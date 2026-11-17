// The admins' count beside the navigation (navBadges): downloads in
// progress. The list comes once from x/admin/torrents, then from every
// addon.torrents.admin.progress (the whole download list, every 2 s while
// anyone listens); the adults' addon.torrents.progress is another list and
// is not counted here. Quietly 0 on any error. The core renders the badge
// for admins only (admin: true) and hides it at 0.
import { useEvents } from '@antresol/addon-sdk'
import { useEffect, useRef, useState } from 'react'
import { getTorrents } from '../api'
import type { Torrent } from '../types'

export const ADMIN_PROGRESS = 'addon.torrents.admin.progress'
const EVENTS = [ADMIN_PROGRESS]
const ACTIVE = new Set(['queued', 'downloading', 'waiting_space'])

const countOf = (list: unknown) => (Array.isArray(list) ? (list as Torrent[]).filter((t) => ACTIVE.has(t.status)).length : 0)

export function useActiveDownloads(): number {
  const [count, setCount] = useState(0)
  // an event's list is newer than the first answer, whenever that comes
  const evented = useRef(false)
  useEffect(() => {
    const ac = new AbortController()
    getTorrents(ac.signal).then(
      (list) => !ac.signal.aborted && !evented.current && setCount(countOf(list)),
      () => !ac.signal.aborted && !evented.current && setCount(0),
    )
    return () => ac.abort()
  }, [])
  useEvents((e) => {
    if (e.type !== ADMIN_PROGRESS || !Array.isArray(e.data)) return
    evented.current = true
    setCount(countOf(e.data))
  }, EVENTS)
  return count
}
