// «Докачивать новые» on a title page: the follow comes from the addon
// (x/follow/by-tmdb) and is read again when its check ends.
import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { listenEvent } from '@antresol/test-core/api/events'
import { session } from '@antresol/test-core/state/session'
import { UNDO_MS, useFollow } from '../useFollow'
import { FOLLOW_CHECKED } from '../events'

const json = (b: unknown, status = 200) => new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const followed = { id: 9, kind: 'series', tmdbId: 5, title: 'Сёгун', poster: '', active: true, lastChecked: null, lastError: '', missing: 0, addedBy: 'Мама', mine: true }

/** A stand-in for the one shared EventSource (jsdom has none). */
class FakeSource {
  static last: FakeSource | null = null
  listeners = new Map<string, ((m: MessageEvent) => void)[]>()
  onmessage: ((m: MessageEvent) => void) | null = null
  constructor() {
    FakeSource.last = this
  }
  addEventListener(type: string, fn: (m: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn])
  }
  close() {}
  emit(type: string, data: unknown) {
    for (const fn of this.listeners.get(type) ?? []) fn(new MessageEvent(type, { data: JSON.stringify(data) }))
  }
}
const emitForTests = (type: string, data: unknown) => FakeSource.last!.emit(type, data)
listenEvent(FOLLOW_CHECKED)

let byTmdb: () => Response
const mama = { id: 2, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
beforeEach(async () => {
  vi.stubGlobal('EventSource', FakeSource)
  byTmdb = () => json(followed)
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const u = String(input)
      if (u.endsWith('/api/login')) return json(mama)
      return u.includes('/x/follow/by-tmdb') ? byTmdb() : new Response(null, { status: 204 })
    }),
  )
  session.reset()
  await session.login(2)
  vi.mocked(fetch).mockClear()
})

afterEach(() => {
  session.reset()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const deletes = () => vi.mocked(fetch).mock.calls.filter(([u, i]) => String(u) === '/api/addons/torrents/x/follow/9' && (i as RequestInit).method === 'DELETE')

describe('useFollow', () => {
  it('reads the title’s follow from the addon; none (404) is «not followed»', async () => {
    const { result } = renderHook(() => useFollow('series', 5))
    expect(result.current.available).toBe(false)
    await waitFor(() => expect(result.current.following).toBe(true))
    expect(result.current.followedBy).toBeNull()
    expect(String(vi.mocked(fetch).mock.calls[0]?.[0])).toBe('/api/addons/torrents/x/follow/by-tmdb?kind=series&tmdbId=5')

    byTmdb = () => json({ error: 'Не найдено' }, 404)
    const other = renderHook(() => useFollow('movie', 6))
    await waitFor(() => expect(other.result.current.available).toBe(true))
    expect(other.result.current.following).toBe(false)
  })

  it('another profile’s follow is theirs: shown, not changed', async () => {
    byTmdb = () => json({ ...followed, addedBy: 'Папа', mine: false })
    const { result } = renderHook(() => useFollow('series', 5))
    await waitFor(() => expect(result.current.followedBy).toBe('Папа'))
    await act(() => result.current.toggle())
    expect(result.current.following).toBe(true)
    expect(deletes()).toHaveLength(0)
  })

  it('a follow of another profile with this profile’s name is still theirs (by id)', async () => {
    byTmdb = () => json({ ...followed, addedBy: 'Мама', mine: false })
    const { result } = renderHook(() => useFollow('series', 5))
    await waitFor(() => expect(result.current.followedBy).toBe('Мама'))
    await act(() => result.current.toggle())
    expect(deletes()).toHaveLength(0)
  })

  it('an own follow with no author name (the profile list unreadable) is still this profile’s', async () => {
    byTmdb = () => json({ ...followed, addedBy: '', mine: true })
    const { result } = renderHook(() => useFollow('series', 5))
    await waitFor(() => expect(result.current.following).toBe(true))
    expect(result.current.followedBy).toBeNull()
  })

  it('a server that cannot follow shows no button', async () => {
    byTmdb = () => json({ error: 'Расширение запускается' }, 503)
    const { result } = renderHook(() => useFollow('series', 5))
    await act(() => new Promise((r) => setTimeout(r, 20)))
    expect(result.current.available).toBe(false)
  })

  it('follow-checked of this follow reads it again', async () => {
    const { result } = renderHook(() => useFollow('series', 5))
    await waitFor(() => expect(result.current.following).toBe(true))
    byTmdb = () => json({ ...followed, active: false })
    act(() => emitForTests(FOLLOW_CHECKED, { followId: 8 }))
    await act(() => new Promise((r) => setTimeout(r, 20)))
    expect(result.current.following).toBe(true)
    act(() => emitForTests(FOLLOW_CHECKED, { followId: 9 }))
    await waitFor(() => expect(result.current.following).toBe(false))
  })

  it('a reload inside the undo window does not bring «Слежу» back', async () => {
    const { result } = renderHook(() => useFollow('series', 5))
    await waitFor(() => expect(result.current.following).toBe(true))
    vi.useFakeTimers({ shouldAdvanceTime: true })
    await act(() => result.current.toggle())
    expect(result.current.following).toBe(false)
    expect(result.current.undoable).toBe(true)

    // the check ended meanwhile: the server still has the follow
    act(() => emitForTests(FOLLOW_CHECKED, { followId: 9 }))
    await act(() => vi.advanceTimersByTimeAsync(20))
    expect(result.current.following).toBe(false)

    await act(() => vi.advanceTimersByTimeAsync(UNDO_MS))
    expect(deletes()).toHaveLength(1)
    expect(result.current.undoable).toBe(false)
  })

  it('«Отменить» keeps the follow', async () => {
    const { result } = renderHook(() => useFollow('series', 5))
    await waitFor(() => expect(result.current.following).toBe(true))
    vi.useFakeTimers({ shouldAdvanceTime: true })
    await act(() => result.current.toggle())
    act(() => result.current.undo())
    expect(result.current.following).toBe(true)
    await act(() => vi.advanceTimersByTimeAsync(UNDO_MS))
    expect(deletes()).toHaveLength(0)
  })
})
