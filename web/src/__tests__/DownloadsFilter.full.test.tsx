import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router'
import type { Torrent } from '../types'
import { Downloads, filterTorrents, segmentOf, sortTorrents, statusGroup } from '../pages/Downloads'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const base: Omit<Torrent, 'infoHash' | 'name' | 'status'> = {
  mode: 'download', size: 1000, done: 1000, downRate: 0, upRate: 0, uploaded: 0, downloaded: 1000, peers: 0, seeds: 0, ratio: 0,
  itemId: null, addedAt: '2026-10-25T10:00:00Z', saveDir: '', organized: true,
}
const tor = (ih: string, p: Partial<Torrent>): Torrent => ({ ...base, infoHash: ih.repeat(40), name: ih, status: 'seeding', ...p })

const list: Torrent[] = [
  tor('a', { title: 'Дюна', kind: 'movie', watched: true, size: 9000, done: 9000, addedAt: '2026-10-20T10:00:00Z' }),
  tor('b', { title: 'Брат', kind: 'movie', watched: false, size: 3000, done: 3000, addedAt: '2026-10-22T10:00:00Z' }),
  tor('c', { title: 'Сёгун', kind: 'series', status: 'downloading', size: 5000, done: 1000, addedAt: '2026-10-24T10:00:00Z' }),
  tor('d', { title: 'Группа крови', kind: 'album', size: 400, done: 400, addedAt: '2026-10-21T10:00:00Z' }),
  tor('e', { title: 'Мастер и Маргарита', kind: 'audiobook', status: 'paused', size: 800, done: 200, addedAt: '2026-10-23T10:00:00Z' }),
  tor('f', { title: 'Вий', kind: 'movie', status: 'error', error: 'Нет участников', size: 2000, done: 0, addedAt: '2026-10-19T10:00:00Z' }),
]

function Where() {
  return <output data-testid="where">{useLocation().search}</output>
}

function mount(search = '') {
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
    <MemoryRouter initialEntries={['/addons/torrents/downloads' + search]}>
      <Downloads />
      <Where />
    </MemoryRouter>,
  )
}

const names = async () => (await screen.findAllByTestId('torrent-card')).map((c) => c.querySelector('p')!.textContent)
const openMenu = () => userEvent.click(screen.getByRole('button', { name: 'Сортировка и фильтры' }))

describe('Загрузки: sections, sort and filters', () => {
  beforeEach(() => localStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('shows only the sections with downloads and filters by the chosen one', async () => {
    mount()
    const seg = await screen.findByRole('radiogroup', { name: 'Раздел загрузок' })
    // audiobooks are under «Книги», as in the library
    expect(within(seg).getAllByRole('radio').map((r) => r.textContent)).toEqual(['Все', 'Фильмы', 'Сериалы', 'Музыка', 'Книги'])
    expect(await names()).toHaveLength(6)
    await userEvent.click(within(seg).getByRole('radio', { name: 'Фильмы' }))
    expect(await names()).toEqual(['Вий', 'Брат', 'Дюна'])
    expect(screen.getByTestId('where')).toHaveTextContent('?kind=movies')
    // the sections inside a segment stay: errors among the downloads, the rest under «Раздаются»
    expect(screen.getByRole('heading', { name: 'Раздаются' })).toBeInTheDocument()
    await userEvent.click(within(seg).getByRole('radio', { name: 'Все' }))
    expect(screen.getByTestId('where')).toHaveTextContent(/^$/)
  })

  it('keeps the section and the sort from the URL (a reload)', async () => {
    mount('?kind=books&sort=name')
    expect(await names()).toEqual(['Мастер и Маргарита'])
    expect(screen.getByRole('radio', { name: 'Книги' })).toHaveAttribute('aria-checked', 'true')
  })

  it('sorts newest first by default, then by name, size and state from the menu', async () => {
    mount()
    expect(await names()).toEqual(['Сёгун', 'Мастер и Маргарита', 'Вий', 'Брат', 'Группа крови', 'Дюна'])
    await openMenu()
    const menu = screen.getByRole('menu', { name: 'Сортировка и фильтры' })
    expect(within(menu).getByRole('menuitemradio', { name: 'По дате добавления' })).toHaveAttribute('aria-checked', 'true')
    await userEvent.click(within(menu).getByRole('menuitemradio', { name: 'По названию' }))
    // the non-seeding section first (sorted), then «Раздаются» (sorted)
    expect(await names()).toEqual(['Вий', 'Мастер и Маргарита', 'Сёгун', 'Брат', 'Группа крови', 'Дюна'])
    expect(screen.getByTestId('where')).toHaveTextContent('?sort=name')
    await openMenu()
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'По размеру' }))
    expect(await names()).toEqual(['Сёгун', 'Вий', 'Мастер и Маргарита', 'Дюна', 'Брат', 'Группа крови'])
  })

  it('filters by watched (films and series) and by status, with an empty state and a reset', async () => {
    mount()
    await names()
    await openMenu()
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'Просмотренные' }))
    expect(await names()).toEqual(['Дюна'])
    expect(screen.getByTestId('filters-on')).toBeInTheDocument()
    await openMenu()
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'Не просмотренные' }))
    expect(await names()).toEqual(['Брат'])
    await openMenu()
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'С ошибками' }))
    expect(screen.queryAllByTestId('torrent-card')).toHaveLength(0)
    expect(screen.getByRole('heading', { name: 'Нет загрузок в этом разделе' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Сбросить фильтры' }))
    expect(await names()).toHaveLength(6)
    expect(screen.queryByTestId('filters-on')).not.toBeInTheDocument()
  })

  it('offers no watched filter where the server marks nothing', async () => {
    mount('?kind=music')
    await names()
    await openMenu()
    expect(screen.queryByRole('menuitemradio', { name: 'Просмотренные' })).not.toBeInTheDocument()
    expect(screen.getByRole('menuitemradio', { name: 'На паузе' })).toBeInTheDocument()
  })

  it('a chosen section without downloads stays in the switcher with its empty state', async () => {
    mount('?kind=books&status=seeding')
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Нет загрузок в этом разделе' })).toBeInTheDocument())
    expect(screen.getByRole('radio', { name: 'Книги' })).toHaveAttribute('aria-checked', 'true')
  })
})

describe('Загрузки: grouping helpers', () => {
  it('puts every kind in its section; unknown kinds only in «Все»', () => {
    expect(['movie', 'series', 'album', 'music', 'book', 'audiobook', 'comic', undefined].map((k) => segmentOf(k))).toEqual([
      'movies', 'series', 'music', 'music', 'books', 'books', 'books', null,
    ])
    expect(filterTorrents([tor('x', {})], { segment: 'movies', watch: 'all', status: 'all' })).toEqual([])
  })

  it('groups statuses and orders by state: errors, downloading, paused, done', () => {
    expect(list.map(statusGroup)).toEqual(['seeding', 'seeding', 'active', 'seeding', 'paused', 'error'])
    expect(statusGroup(tor('g', { direct: true, status: 'done' }))).toBe('seeding')
    expect(statusGroup(tor('h', { status: 'waiting_space' }))).toBe('paused')
    expect(sortTorrents(list, 'state').map((t) => t.title)).toEqual(['Вий', 'Сёгун', 'Мастер и Маргарита', 'Брат', 'Группа крови', 'Дюна'])
  })
})
