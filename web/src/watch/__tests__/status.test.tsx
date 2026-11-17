// The «Смотреть сейчас» status poll: one request at a time (the next one
// waits for the answer), and none once the download is complete or gone.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { STATUS_POLL_MS, useWatchStatus } from '../status'

const IH = 'f'.repeat(40)
const w = { ih: IH, idx: 0 }
const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const status = (o: object) => ({ status: 'downloading', mode: 'stream', size: 1000, done: 100, downRate: 0, peers: 1, seeds: 1, ...o })

const tick = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)))

describe('useWatchStatus', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('never overlaps: a slow answer holds the next poll', async () => {
    const f = vi.fn(() => new Promise<Response>(() => {}))
    vi.stubGlobal('fetch', f)
    const { unmount } = renderHook(() => useWatchStatus(w))
    await tick(STATUS_POLL_MS * 4)
    expect(f).toHaveBeenCalledTimes(1)
    unmount()
  })

  it('polls on while downloading, and stops once the download is complete', async () => {
    let done = 100
    const f = vi.fn(async () => json(status({ done })))
    vi.stubGlobal('fetch', f)
    const { result, unmount } = renderHook(() => useWatchStatus(w))
    await tick(0)
    await tick(STATUS_POLL_MS)
    expect(f).toHaveBeenCalledTimes(2)
    done = 1000
    await tick(STATUS_POLL_MS)
    expect(result.current?.done).toBe(1000)
    const n = f.mock.calls.length
    await tick(STATUS_POLL_MS * 5)
    expect(f).toHaveBeenCalledTimes(n)
    unmount()
  })

  it('stops when the download is gone (404)', async () => {
    const f = vi.fn(async () => json({ error: 'нет' }, 404))
    vi.stubGlobal('fetch', f)
    const { unmount } = renderHook(() => useWatchStatus(w))
    await tick(0)
    await tick(STATUS_POLL_MS * 5)
    expect(f).toHaveBeenCalledTimes(1)
    unmount()
  })
})
