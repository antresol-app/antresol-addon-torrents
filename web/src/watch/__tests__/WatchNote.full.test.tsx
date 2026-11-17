// The note the core's input player shows while a «Смотреть сейчас» input
// waits for data (playerNotes): how far the download is, and why it makes
// no headway.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import type { PlayerInput } from '@antresol/addon-sdk'
import type { StreamStatus } from '../status'
import { bufferingNote, WatchNote } from '../WatchNote'

const IH = 'c'.repeat(40)
const input = (meta: unknown = { ih: IH, idx: 3, mode: 'stream' }): PlayerInput => ({ id: 'in-1', addonId: 'torrents', name: 'Фильм/Фильм.mkv', itemId: 0, slow: true, meta, startSec: 0 })
const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })

afterEach(() => vi.unstubAllGlobals())

describe('plain words', () => {
  it('tells a waiting viewer how far the torrent is', () => {
    const s: StreamStatus = { status: 'downloading', mode: 'stream', size: 1000, done: 250, downRate: 2_000_000, peers: 5, seeds: 3 }
    // Without the buffer window: the share of the file.
    expect(bufferingNote(s)).toMatch(/^Загружается… 25 % · 5 участников · /)
    // With it: how much of what the player needs next has arrived.
    expect(bufferingNote({ ...s, bufferHave: 6 << 20, bufferWant: 16 << 20 })).toMatch(/^Загружается… 37 % · 5 участников/)
    expect(bufferingNote({ ...s, bufferHave: 16 << 20, bufferWant: 16 << 20, downRate: 0 })).toBe('Загружается… 99 % · 5 участников')
    expect(bufferingNote(null)).toBe('Подключаемся к раздаче…')
    expect(bufferingNote({ ...s, status: 'queued', size: 0, peers: 0 })).toBe('Ищем, у кого есть этот фильм…')
  })
})

describe('WatchNote', () => {
  it('stalled hint while waiting', async () => {
    const f = vi.fn(async () => json({ status: 'downloading', mode: 'stream', size: 1000, done: 0, downRate: 0, peers: 0, seeds: 0, stalled: 'no_peers' }))
    vi.stubGlobal('fetch', f)
    render(<WatchNote input={input()} />)
    expect(screen.getByText('Подключаемся к раздаче…')).toBeInTheDocument()
    expect(await screen.findByText('Нет участников раздачи — ждём…')).toBeInTheDocument()
    expect(String(f.mock.calls[0]![0])).toBe(`/api/addons/torrents/x/torrents/${IH}/status?file=3`)
  })

  it('an input that is not a download shows the plain note and asks nothing', () => {
    const f = vi.fn()
    vi.stubGlobal('fetch', f)
    render(<WatchNote input={input(null)} />)
    expect(screen.getByText('Подключаемся к раздаче…')).toBeInTheDocument()
    expect(f.mock.calls.map(([u]) => String(u))).toEqual([])
  })
})
