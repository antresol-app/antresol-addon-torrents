import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { Downloads } from '../pages/Downloads'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const base: Omit<Torrent, 'infoHash' | 'name' | 'status'> = {
  mode: 'download', size: 1000, done: 1000, downRate: 0, upRate: 0, uploaded: 0, downloaded: 1000, peers: 0, seeds: 0, ratio: 0,
  itemId: null, addedAt: '2026-10-25T10:00:00Z', saveDir: '', organized: true,
}

describe('Загрузки: notes and direct downloads', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the organize note and a direct download without pause', async () => {
    const list: Torrent[] = [
      { ...base, infoHash: 'a'.repeat(40), name: 'Shogun.S01E02', title: 'Сёгун', kind: 'series', status: 'seeding', note: 'Уже есть в медиатеке' },
      { ...base, infoHash: 'direct-1', name: 'master.epub', title: 'Булгаков — Мастер и Маргарита', kind: 'book', status: 'done', direct: true },
      { ...base, infoHash: 'direct-2', name: 'heart.fb2', title: 'Собачье сердце', kind: 'book', status: 'queued', direct: true, done: 0, size: -1 },
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
    const seeding = cards.find((c) => c.textContent?.includes('Сёгун'))!
    expect(within(seeding).getByText('Уже есть в медиатеке')).toBeInTheDocument()
    const done = cards.find((c) => c.textContent?.includes('Мастер и Маргарита'))!
    expect(within(done).getByText('Загружено')).toBeInTheDocument()
    expect(within(done).getByText('прямая загрузка')).toBeInTheDocument()
    expect(within(done).queryByRole('button', { name: 'Пауза' })).not.toBeInTheDocument()
    const queued = cards.find((c) => c.textContent?.includes('Собачье сердце'))!
    expect(within(queued).getByText('В очереди')).toBeInTheDocument()
    expect(within(queued).queryByRole('button', { name: /Пауза|Продолжить/ })).not.toBeInTheDocument()
  })
})

describe('Загрузки: another album', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('offers «Разложить всё равно» on «Скачан другой альбом» and forces organize', async () => {
    const ih = 'b'.repeat(40)
    const list: Torrent[] = [
      { ...base, infoHash: ih, name: 'Metallica - 72 Seasons', title: 'Metallica', kind: 'music', status: 'seeding', organized: false, organizeError: 'Скачан другой альбом: 72 Seasons (2023)' },
      { ...base, infoHash: 'c'.repeat(40), name: 'Кино', title: 'Группа крови', kind: 'music', status: 'seeding', organized: false, organizeError: 'Нет доступа к папке' },
    ]
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/addons/torrents/x/admin/torrents')) return json(list)
      if (url.endsWith('/api/disk')) return json({ total: 1, used: 0, free: 1 })
      if (init?.method === 'POST') return new Response(null, { status: 204 })
      return json({})
    })
    vi.stubGlobal('fetch', f)
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const cards = await screen.findAllByTestId('torrent-card')
    const other = cards.find((c) => c.textContent?.includes('72 Seasons'))!
    const plain = cards.find((c) => c.textContent?.includes('Группа крови'))!
    expect(within(plain).queryByRole('button', { name: 'Разложить всё равно' })).not.toBeInTheDocument()
    fireEvent.click(within(other).getByRole('button', { name: 'Разложить всё равно' }))
    await waitFor(() =>
      expect(f.mock.calls.some(([u, i]) => String(u).endsWith(`/api/addons/torrents/x/admin/torrents/${ih}/organize-force`) && i?.method === 'POST')).toBe(true),
    )
  })
})
