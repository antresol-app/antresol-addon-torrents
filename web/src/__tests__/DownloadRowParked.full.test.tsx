import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { rowState } from '../pages/DownloadRow'
import { statusGroup, Downloads } from '../pages/Downloads'

const row = (over: Partial<Torrent>) =>
  ({ infoHash: 'a', name: 'Альбом', mode: 'download', status: 'seeding', size: 100, done: 100, downRate: 0, upRate: 0, peers: 0, seeds: 0, uploaded: 0, downloaded: 100, ratio: 0, addedAt: '2026-11-05T00:00:00Z', ...over }) as Torrent
const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })

describe('finished torrents with seeding off', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('are done rows, in the same filter group', () => {
    expect(rowState(row({ parked: true }))).toBe('done')
    expect(rowState(row({}))).toBe('seeding')
    expect(statusGroup(row({ parked: true }))).toBe('seeding')
  })

  it('are listed apart from what is seeding, and never called seeding', async () => {
    const list = [row({ infoHash: 'a', name: 'Готовый', parked: true }), row({ infoHash: 'b', name: 'Раздаваемый' })]
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => json(String(input).includes('/api/addons/torrents/x/admin/torrents') ? list : {})))
    render(<MemoryRouter><Downloads /></MemoryRouter>)
    const card = (await screen.findByText('Готовый')).closest('[data-testid="torrent-card"]') as HTMLElement
    expect(card).toHaveAttribute('data-state', 'done')
    expect(within(card).queryByText('Раздаётся')).not.toBeInTheDocument()
    expect(within(card).getByText('Скачано')).toBeInTheDocument()
    expect(await screen.findByText('Скачанные')).toBeInTheDocument()
    expect(screen.getByText('Раздаётся: 1 · Загружено: 1', { exact: false })).toBeInTheDocument()
  })
})
