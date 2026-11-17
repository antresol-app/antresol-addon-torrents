// The download ring of a title: it follows the adults' `progress` event
// and asks the server only while no event came for a while.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'

type Ev = { type: string; data: unknown }
let emit: ((e: Ev) => void) | null = null
let listened: readonly string[] | undefined
vi.mock('@antresol/addon-sdk', async (orig) => ({
  ...(await orig<typeof import('@antresol/addon-sdk')>()),
  useEvents: (h: (e: Ev) => void, types?: readonly string[]) => {
    emit = (e) => (!types || types.includes(e.type)) && h(e)
    listened = types
  },
}))

import { EVENT_FRESH_MS, POLL_MS, useTitleProgress } from '../shared/useTitleProgress'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const progressCalls = (f: ReturnType<typeof vi.fn>) => f.mock.calls.filter(([u]) => String(u).includes('/x/acquire/progress')).length
const entry = (o: object) => ({ key: 'movie:1', kind: 'movie', tmdbId: 1, season: 0, status: 'active', done: 10, size: 100, ...o })

describe('useTitleProgress', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
    emit = null
  })

  it('follows the progress event', async () => {
    const f = vi.fn(async () => json({ status: 'active', done: 5, size: 100 }))
    vi.stubGlobal('fetch', f)
    const { result } = renderHook(() => useTitleProgress({ kind: 'movie', tmdbId: 1 }))
    await act(async () => void (await vi.advanceTimersByTimeAsync(0)))
    expect(result.current.state).toEqual({ kind: 'active', done: 5, size: 100, stalled: undefined, hint: undefined })
    expect(progressCalls(f)).toBe(1)
    // only the adults' event: an admin's admin.progress is another list
    expect(listened).toEqual(['addon.torrents.progress'])

    act(() => emit!({ type: 'addon.torrents.progress', data: [entry({ tmdbId: 2, done: 99 }), entry({ done: 40 })] }))
    expect(result.current.state).toMatchObject({ kind: 'active', done: 40, size: 100 })
    // events keep coming: the poll stays quiet
    for (let i = 0; i < 4; i++) {
      await act(async () => void (await vi.advanceTimersByTimeAsync(POLL_MS)))
      act(() => emit!({ type: 'addon.torrents.progress', data: [entry({ done: 50 + i })] }))
    }
    expect(progressCalls(f)).toBe(1)
    expect(result.current.state).toMatchObject({ done: 53 })

    act(() => emit!({ type: 'addon.torrents.progress', data: [entry({ status: 'paused', done: 60 })] }))
    expect(result.current.state).toEqual({ kind: 'paused', done: 60, size: 100 })

    // the events stop: after a while the ring asks the server again
    await act(async () => void (await vi.advanceTimersByTimeAsync(EVENT_FRESH_MS + POLL_MS)))
    expect(progressCalls(f)).toBeGreaterThan(1)
  })

  it('a title the event no longer lists is asked once (done or gone)', async () => {
    let answer: unknown = { status: 'active', done: 5, size: 100 }
    const f = vi.fn(async () => (answer === null ? new Response(null, { status: 204 }) : json(answer)))
    vi.stubGlobal('fetch', f)
    const { result } = renderHook(() => useTitleProgress({ kind: 'movie', tmdbId: 1 }))
    await act(async () => void (await vi.advanceTimersByTimeAsync(0)))
    answer = { status: 'done', done: 100, size: 100 }
    await act(async () => {
      emit!({ type: 'addon.torrents.progress', data: [] })
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(result.current.state).toEqual({ kind: 'done' })
    expect(progressCalls(f)).toBe(2)
  })

  it('the cards of one render ask in one request (a search of many results)', async () => {
    const f = vi.fn(async (u: string) =>
      String(u).includes('/batch') ? json({ 'movie:1': { status: 'active', done: 5, size: 100 }, 'movie:3': { status: 'done', done: 9, size: 9 } }) : json({}),
    )
    vi.stubGlobal('fetch', f)
    const { result } = renderHook(() => [1, 2, 3].map((id) => useTitleProgress({ kind: 'movie', tmdbId: id }).state))
    await act(async () => void (await vi.advanceTimersByTimeAsync(0)))
    expect(progressCalls(f)).toBe(1)
    const url = String(f.mock.calls[0][0])
    expect(url).toContain('/x/acquire/progress/batch?')
    expect(new URL(url, 'http://x').searchParams.getAll('t')).toEqual(['movie:1', 'movie:2', 'movie:3'])
    expect(result.current).toEqual([{ kind: 'active', done: 5, size: 100, stalled: undefined, hint: undefined }, { kind: 'idle' }, { kind: 'done' }])
  })
})
