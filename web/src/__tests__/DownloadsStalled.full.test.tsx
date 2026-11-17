// A download that makes no headway says why (torrent.Status.Stalled) and
// offers another release on the title's page.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { Downloads, replacePath } from '../pages/Downloads'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const base: Omit<Torrent, 'infoHash' | 'name' | 'status'> = {
  mode: 'download', size: 1000, done: 0, downRate: 0, upRate: 0, uploaded: 0, downloaded: 0, peers: 0, seeds: 0, ratio: 0,
  itemId: null, addedAt: '2026-10-25T10:00:00Z', saveDir: '', organized: false,
}

describe('Загрузки: stalled downloads', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('a row without peers says so and links to another release of its title', async () => {
    const list: Torrent[] = [
      { ...base, infoHash: 'a'.repeat(40), name: 'Shogun.S02', title: 'Сёгун', kind: 'series', status: 'downloading', stalled: 'no_peers', tmdbId: 77, season: 2 },
      { ...base, infoHash: 'b'.repeat(40), name: 'Dune', title: 'Дюна', kind: 'movie', status: 'queued', stalled: 'no_metadata', tmdbId: 42, size: 0 },
      { ...base, infoHash: 'c'.repeat(40), name: 'Brat', title: 'Брат', kind: 'movie', status: 'downloading', tmdbId: 9, peers: 3, done: 500 },
      { ...base, infoHash: 'd'.repeat(40), name: 'Kino', title: 'Группа крови', kind: 'music', status: 'downloading', stalled: 'no_peers' },
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
    const card = (title: string) => cards.find((c) => c.textContent?.includes(title))!
    const shogun = within(card('Сёгун')).getByTestId('stalled-note')
    expect(within(shogun).getByRole('status')).toHaveTextContent('Нет участников раздачи — ждём…')
    expect(within(shogun).getByRole('link', { name: 'Выбрать другую раздачу…' })).toHaveAttribute('href', '/title/series/77?replace=1&season=2')
    const dune = within(card('Дюна')).getByTestId('stalled-note')
    expect(dune).toHaveTextContent('Получение данных о раздаче…')
    expect(within(dune).getByRole('link', { name: 'Выбрать другую раздачу…' })).toHaveAttribute('href', '/title/movie/42?replace=1')
    expect(within(card('Брат')).queryByTestId('stalled-note')).not.toBeInTheDocument()
    // music has no title page to pick from: the line alone
    const album = within(card('Группа крови')).getByTestId('stalled-note')
    expect(album).toHaveTextContent('Нет участников раздачи — ждём…')
    expect(within(album).queryByRole('link')).not.toBeInTheDocument()
  })

  it('replacePath: only a movie or series download without a library page', () => {
    const t = { ...base, infoHash: 'a', name: 'x', status: 'downloading', kind: 'movie', tmdbId: 5 } as Torrent
    expect(replacePath(t)).toBe('/title/movie/5?replace=1')
    expect(replacePath({ ...t, kind: 'series', season: 3 })).toBe('/title/series/5?replace=1&season=3')
    // a whole-series pack (season 0) is «Все сезоны», not season 1
    expect(replacePath({ ...t, kind: 'series', season: 0 })).toBe('/title/series/5?replace=1&season=all')
    expect(replacePath({ ...t, kind: 'series', season: undefined })).toBe('/title/series/5?replace=1&season=all')
    expect(replacePath({ ...t, mode: 'stream' })).toBeUndefined()
    expect(replacePath({ ...t, itemId: 12 })).toBeUndefined()
    expect(replacePath({ ...t, tmdbId: undefined })).toBeUndefined()
    expect(replacePath({ ...t, kind: 'book' })).toBeUndefined()
  })
})

describe('Загрузки: dead releases', () => {
  afterEach(() => vi.unstubAllGlobals())

  function stubList(list: Torrent[], calls: string[]) {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        if (init?.method === 'POST') {
          calls.push(url)
          return new Response(null, { status: 204 })
        }
        if (url.endsWith('/api/addons/torrents/x/admin/torrents')) return json(list)
        if (url.endsWith('/api/disk')) return json({ total: 1, used: 0, free: 1 })
        return json({})
      }),
    )
  }

  it('says what the tracker answered, what the probe saw and what Antresol did', async () => {
    const calls: string[] = []
    stubList(
      [
        { ...base, infoHash: 'a'.repeat(40), name: 'Shogun.S02', title: 'Сёгун', kind: 'series', status: 'downloading', stalled: 'no_peers', trackerError: 'Неверный (34 - пасскей)', trackerErrorKind: 'bad_passkey', tmdbId: 77, season: 2 },
        { ...base, infoHash: 'b'.repeat(40), name: 'Dune', title: 'Дюна', kind: 'movie', status: 'queued', stalled: 'no_peers', trackerErrorKind: 'not_registered', tmdbId: 42 },
        { ...base, infoHash: 'c'.repeat(40), name: 'Brat', title: 'Брат', kind: 'movie', status: 'downloading', stalled: 'no_peers', trackerError: 'go away', trackerErrorKind: 'rejected', tmdbId: 9 },
        { ...base, infoHash: 'd'.repeat(40), name: 'Kin', title: 'Кин-дза-дза', kind: 'movie', status: 'downloading', liveness: 'unseen', tmdbId: 10 },
        { ...base, infoHash: 'e'.repeat(40), name: 'Stalker.1979.1080p', title: 'Сталкер', kind: 'movie', status: 'downloading', liveness: 'checking', autoReplace: 'switched', tmdbId: 11 },
        { ...base, infoHash: 'f'.repeat(40), name: 'Solaris', title: 'Солярис', kind: 'movie', status: 'downloading', stalled: 'no_peers', liveness: 'unseen', autoReplace: 'none_alive', tmdbId: 12 },
        { ...base, infoHash: '1'.repeat(40), name: 'Mirror', title: 'Зеркало', kind: 'movie', status: 'downloading', liveness: 'alive', peers: 2, done: 10, tmdbId: 13 },
      ],
      calls,
    )
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const cards = await screen.findAllByTestId('torrent-card')
    const card = (title: string) => cards.find((c) => c.textContent?.includes(title))!
    expect(within(card('Сёгун')).getByTestId('stalled-note')).toHaveTextContent('Трекер отклонил раздачу: неверный пасскей')
    expect(within(card('Дюна')).getByTestId('stalled-note')).toHaveTextContent('Раздача не зарегистрирована на трекере')
    expect(within(card('Брат')).getByTestId('stalled-note')).toHaveTextContent('Трекер ответил: go away')
    // the probe found no seeds: said at once, with the way out
    const kin = within(card('Кин-дза-дза')).getByTestId('stalled-note')
    expect(kin).toHaveTextContent('Сидов пока не видно')
    expect(within(kin).getByRole('link', { name: 'Выбрать другую раздачу…' })).toHaveAttribute('href', '/title/movie/10?replace=1')
    // still checking, or alive: quiet
    expect(within(card('Сталкер')).queryByTestId('stalled-note')).not.toBeInTheDocument()
    expect(within(card('Зеркало')).queryByTestId('stalled-note')).not.toBeInTheDocument()
    const solaris = within(card('Солярис')).getByTestId('stalled-note')
    expect(solaris).toHaveTextContent('Живых раздач не нашлось')
    expect(within(solaris).getByRole('link', { name: 'Выбрать другую раздачу…' })).toBeInTheDocument()

    const switched = within(card('Сталкер')).getByTestId('auto-switched')
    expect(switched).toHaveTextContent('Раздача не отвечала — Antresol переключился на «Stalker.1979.1080p»')
    fireEvent.click(within(switched).getByRole('button', { name: 'Скрыть' }))
    await waitFor(() => expect(calls.some((u) => u.endsWith(`/api/addons/torrents/x/admin/torrents/${'e'.repeat(40)}/dismiss-note`))).toBe(true))
  })
})
