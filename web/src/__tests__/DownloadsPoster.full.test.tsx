import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { Downloads } from '../pages/Downloads'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const base: Omit<Torrent, 'infoHash' | 'name' | 'status'> = {
  mode: 'download', size: 1000, done: 400, downRate: 10, upRate: 0, uploaded: 0, downloaded: 400, peers: 1, seeds: 1, ratio: 0,
  itemId: null, addedAt: '2026-10-25T10:00:00Z', saveDir: '', organized: false,
}

describe('Загрузки: covers', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    localStorage.clear()
  })

  it('shows the server-kept cover from the image cache, never a third-party host', async () => {
    const cover = 'a'.repeat(40) + '.jpg'
    const list: Torrent[] = [
      { ...base, infoHash: 'a'.repeat(40), name: 'Dune.2024.1080p', title: 'Дюна', kind: 'movie', status: 'downloading', poster: cover },
      { ...base, infoHash: 'b'.repeat(40), name: 'Kino', title: 'Группа крови', kind: 'music', status: 'downloading' },
    ]
    // a poster this browser remembered from a third-party host must not be loaded
    localStorage.setItem('vault.art.' + 'b'.repeat(40), 'https://image.tmdb.org/t/p/w342/x.jpg')
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/addons/torrents/x/admin/torrents')) return json(list)
        if (url.endsWith('/api/disk')) return json({ total: 1, used: 0, free: 1 })
        return json({})
      }),
    )
    const { container } = render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const cards = await screen.findAllByTestId('torrent-card')
    expect(cards).toHaveLength(2)
    const srcs = [...container.querySelectorAll('img')].map((i) => i.getAttribute('src'))
    expect(srcs).toEqual([`/api/images/${cover}?w=160`])
  })
})
