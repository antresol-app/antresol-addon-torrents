// The source progress the input player waits on (playerNotes.useProgress):
// the bytes the download has, grown by what arrived where the player reads.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { PlayerInput } from '@antresol/addon-sdk'
import { STATUS_POLL_MS } from '../status'
import { useWatchProgress } from '../useWatchProgress'

const IH = 'e'.repeat(40)
const input: PlayerInput = { id: 'in-1', addonId: 'torrents', name: 'Фильм.mkv', itemId: 0, slow: true, meta: { ih: IH, idx: 1, mode: 'stream' }, startSec: 0 }
const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })

describe('useWatchProgress', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('grows with done + bufferHave', async () => {
    const answers = [
      { done: 100, bufferHave: 10 },
      { done: 200, bufferHave: 50 },
      // a new buffer window starts empty: the progress never goes back
      { done: 210, bufferHave: 0 },
    ]
    let n = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => json({ status: 'downloading', mode: 'stream', size: 1000, downRate: 0, peers: 1, seeds: 1, ...answers[Math.min(n++, answers.length - 1)] })),
    )
    const { result } = renderHook(() => useWatchProgress(input))
    expect(result.current).toBe(0)
    await act(async () => void (await vi.advanceTimersByTimeAsync(0)))
    expect(result.current).toBe(110)
    await act(async () => void (await vi.advanceTimersByTimeAsync(STATUS_POLL_MS)))
    expect(result.current).toBe(250)
    await act(async () => void (await vi.advanceTimersByTimeAsync(STATUS_POLL_MS)))
    expect(result.current).toBe(250)
  })
})
