import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { Downloads } from '../pages/Downloads'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const base: Omit<Torrent, 'infoHash' | 'name' | 'status' | 'title'> = {
  mode: 'download', size: 1000, done: 100, downRate: 0, upRate: 0, uploaded: 0, downloaded: 100, peers: 0, seeds: 0, ratio: 0,
  itemId: null, addedAt: '2026-10-25T10:00:00Z', saveDir: '', organized: false, kind: 'music',
}

describe('Загрузки: a partial album', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows «Файлов: N из M» only for a partial selection', async () => {
    const list: Torrent[] = [
      { ...base, infoHash: 'a'.repeat(40), name: 'Kino', title: 'Группа крови', status: 'downloading', selectedFiles: 5, totalFiles: 6 },
      { ...base, infoHash: 'b'.repeat(40), name: 'Kino2', title: 'Звезда по имени Солнце', status: 'downloading', selectedFiles: 6, totalFiles: 6 },
    ]
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/addons/torrents/x/admin/torrents')) return json(list)
        if (url.endsWith('/api/disk')) return json({ total: 1, used: 0, free: 1 })
        return json({})
      }),
    )
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const cards = await screen.findAllByTestId('torrent-card')
    const partial = cards.find((c) => c.textContent?.includes('Группа крови'))!
    expect(within(partial).getByText('Файлов: 5 из 6')).toBeInTheDocument()
    const whole = cards.find((c) => c.textContent?.includes('Звезда по имени Солнце'))!
    expect(within(whole).queryByText(/Файлов:/)).not.toBeInTheDocument()
  })

  it('names some songs of an album «Альбом — 2 трека из 12»', async () => {
    const list: Torrent[] = [
      { ...base, infoHash: 'a'.repeat(40), name: 'Kino', title: 'Группа крови', status: 'downloading', selectedFiles: 3, totalFiles: 14, selectedTracks: 2, totalTracks: 12 },
      { ...base, infoHash: 'b'.repeat(40), name: 'Kino2', title: 'Ночь', status: 'downloading', selectedFiles: 2, totalFiles: 14, selectedTracks: 1, totalTracks: 12 },
      { ...base, infoHash: 'c'.repeat(40), name: 'Kino3', title: 'Звезда по имени Солнце', status: 'downloading', selectedFiles: 6, totalFiles: 6 },
    ]
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/addons/torrents/x/admin/torrents')) return json(list)
        if (url.endsWith('/api/disk')) return json({ total: 1, used: 0, free: 1 })
        return json({})
      }),
    )
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    expect(await screen.findByText('Группа крови — 2 трека из 12')).toBeInTheDocument()
    expect(screen.getByText('Ночь — 1 трек из 12')).toBeInTheDocument()
    // the song count says it all: no «Файлов: N из M» next to it
    expect(screen.queryByText(/Файлов:/)).not.toBeInTheDocument()
    expect(screen.getByText('Звезда по имени Солнце')).toBeInTheDocument()
  })
})
