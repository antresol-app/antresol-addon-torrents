// «Одобрить» in «Запросы» (requestActions): the release picker of the
// requested title; the core hears onApproved once the download was added,
// onClose when the admin cancels.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { KidRequest } from '@antresol/addon-sdk'
import { RequestReleaseSheet } from '../find/RequestReleaseSheet'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

const request = (o: Partial<KidRequest>): KidRequest => ({
  id: 7, key: 'tmdb:1', kind: 'movie', tmdbId: 1, extId: '', title: 'Дюна', subtitle: '', year: 2021, poster: '', aliases: [],
  rating: '', status: 'pending', reason: '', libraryItemId: null, createdAt: '2026-11-15T10:00:00Z', ...o,
} as KidRequest)

const PACK = { title: 'Тестовый сериал / Test Show (2023) S01 WEB-DL 1080p', link: 'http://site/dl/1.torrent', size: 3 << 30, seeders: 9, leechers: 0, sourceId: 1, sourceName: 'Сайт', season: 1, seasonPack: true }

function stub(acquired = true, releases: unknown[] = []) {
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/x/releases')) return json({ releases, rejected: [], failed: [], pending: [], done: true })
    if (url.endsWith('/x/acquire') && init?.method === 'POST')
      return acquired ? json({ infoHash: 'a'.repeat(40), fileIndex: 0, streamUrl: '', status: 'downloading' }) : json({ error: 'Нет раздач' }, 404)
    return json({})
  })
  vi.stubGlobal('fetch', f)
  return f
}

const renderSheet = (r: KidRequest) => {
  const onClose = vi.fn()
  const onApproved = vi.fn()
  render(
    <MemoryRouter>
      <RequestReleaseSheet request={r} onClose={onClose} onApproved={onApproved} />
    </MemoryRouter>,
  )
  return { onClose, onApproved }
}

afterEach(() => vi.unstubAllGlobals())

describe('RequestReleaseSheet', () => {
  it('calls onApproved after the add', async () => {
    const f = stub()
    const { onClose, onApproved } = renderSheet(request({}))
    const sheet = await screen.findByRole('dialog', { name: /Дюна \(2021\)/ })
    await userEvent.click(within(sheet).getAllByRole('button', { name: 'Загрузить' })[0]!)
    await waitFor(() => expect(onApproved).toHaveBeenCalledTimes(1))
    expect(onClose).not.toHaveBeenCalled()
    const [, init] = f.mock.calls.find(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')!
    expect(JSON.parse(String(init!.body))).toEqual({ tmdbId: 1, kind: 'movie', mode: 'download' })
  })

  it('an album: the best release approves too', async () => {
    stub()
    const { onClose, onApproved } = renderSheet(request({ kind: 'music', key: 'ext:mb-1', tmdbId: 0, extId: 'mb-1', title: 'Группа крови', subtitle: 'Кино', year: 1988 }))
    const sheet = await screen.findByRole('dialog', { name: /Кино — Группа крови \(1988\)/ })
    // the «Лучший релиз» row (the server picks, as «На сервер» does)
    await userEvent.click(sheet.querySelector<HTMLButtonElement>('[data-best] button')!)
    await waitFor(() => expect(onApproved).toHaveBeenCalledTimes(1))
    expect(onClose).not.toHaveBeenCalled()
  })

  // the core no longer routes a series to its title page: the sheet is the
  // series' release table by its TMDB id (a season is picked by hand: the
  // server never picks a whole series)
  it('a series: its releases by TMDB id, no best row; picking one approves', async () => {
    const f = stub(true, [PACK])
    const { onClose, onApproved } = renderSheet(request({ kind: 'series', key: 'tmdb:series:4343', tmdbId: 4343, title: 'Тестовый сериал', year: 2023 }))
    const sheet = await screen.findByRole('dialog', { name: /Тестовый сериал \(2023\)/ })
    await within(sheet).findByText(PACK.title)
    const asked = f.mock.calls.map(([u]) => String(u)).find((u) => u.includes('/x/releases'))!
    expect(asked).toContain('tmdbId=4343')
    expect(asked).toContain('kind=series')
    expect(sheet.querySelector('[data-best]')).toBeNull()
    expect(within(sheet).queryByRole('alert')).toBeNull()
    await userEvent.click(within(sheet).getAllByRole('button', { name: 'Загрузить' })[0]!)
    await waitFor(() => expect(onApproved).toHaveBeenCalledTimes(1))
    expect(onClose).not.toHaveBeenCalled()
    const [, init] = f.mock.calls.find(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')!
    expect(JSON.parse(String(init!.body))).toMatchObject({ tmdbId: 4343, kind: 'series', mode: 'download', releaseLink: PACK.link })
  })

  it('a failed add approves nothing; cancel is onClose', async () => {
    stub(false)
    const { onClose, onApproved } = renderSheet(request({}))
    const sheet = await screen.findByRole('dialog', { name: /Дюна \(2021\)/ })
    await userEvent.click(within(sheet).getAllByRole('button', { name: 'Загрузить' })[0]!)
    expect(await within(sheet).findByRole('alert')).toHaveTextContent('Нет раздач')
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(onApproved).not.toHaveBeenCalled()
  })
})
