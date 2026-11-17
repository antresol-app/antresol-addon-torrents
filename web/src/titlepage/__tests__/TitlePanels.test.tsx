// The addon's panel under the core page's action row (itemPanels): on a
// title page (item id 0) «Следить» and the pinned release, no «Добавить
// сезон…»; on a library series «Добавить сезон…» for admins; the follow
// state from x/follow/by-tmdb. Moved here from the tests of the
// old page (ItemDetailFull, Follows.full's title cases).
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { resetAddonsForTests } from '@antresol/test-registry'
import { session } from '@antresol/test-core/state/session'
import type { Pin } from '@antresol/test-addon/types'
import { missingSeasons } from '../TitlePanels'
import { admin, adult, json, kid, posts, registerTheAddon, renderAt, serve, signIn, X } from './page'

const show = {
  tmdbId: 77, kind: 'series', title: 'Дальний берег', year: 2024, poster: '/p77.jpg', backdrop: '', overview: '', inLibrary: false,
  libraryItemId: null, genres: [], seasons: [{ number: 1, episodes: 10 }, { number: 2, episodes: 10 }],
}
const film = { tmdbId: 42, kind: 'movie', title: 'Тихая гавань', year: 2026, poster: '', backdrop: '', overview: '', inLibrary: false, libraryItemId: null, genres: [] }
const pinOf = (over: Partial<Pin> = {}): Pin => ({
  sourceId: '5', link: 'https://t/far', details: '', releaseTitle: 'Far.Shore.S01.1080p.WEB-DL', infoHash: 'a'.repeat(40), quality: '1080p',
  episodes: 8, pinnedAt: '2026-11-01T10:00:00Z', lastChecked: '2026-11-13T10:00:00Z', lastError: '', suggestion: null, ...over,
})
const follow = (over: object = {}) => ({ id: 9, kind: 'series', tmdbId: 77, title: '', poster: '', active: true, source: 'vault', lastChecked: null, lastError: '', missing: 0, addedBy: 'Мама', mine: true, resolution: '', ...over })
const libSeries = {
  id: 10, kind: 'series', title: 'Дальний берег', subtitle: '', poster: '', backdrop: '', logo: '', progress: 0, onServer: true,
  overview: '', genres: [], cast: [], files: [], inWatchlist: false, tmdbId: 77,
  seasons: [{ id: 11, number: 1, title: 'Сезон 1', episodes: [{ id: 12, kind: 'episode', title: 'Серия 1', subtitle: '', poster: '', backdrop: '', logo: '', progress: 0, onServer: true, seasonNo: 1, episodeNo: 1, watched: false }] }],
  actions: { play: true, toDevice: false, toServer: false, watchNow: false },
}

beforeAll(registerTheAddon)
afterAll(() => resetAddonsForTests())
beforeEach(() => localStorage.clear())
afterEach(() => {
  vi.unstubAllGlobals()
  session.reset()
})

async function title(user: { id: number }, path: string, answer?: Parameters<typeof serve>[1], t: object = path.includes('series') ? show : film) {
  const calls = serve(user, answer, { title: t })
  await signIn(user)
  renderAt(path)
  await screen.findByRole('heading', { name: (t as { title: string }).title })
  return calls
}

describe('the title panel on a title page', () => {
  it('«Следить» and the pinned release; no «Добавить сезон…»', async () => {
    const calls = await title(adult, '/title/series/77', (url, m) => (url.endsWith(`${X}pinned/series/77`) && m === 'GET' ? json({ pin: pinOf() }) : undefined))
    expect(await screen.findByRole('button', { name: 'Докачивать новые', pressed: false })).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: 'Закреплённая раздача' })).toBeInTheDocument()
    expect(screen.getByText('Far.Shore.S01.1080p.WEB-DL')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Добавить сезон…' })).not.toBeInTheDocument()
    // the follow comes from the addon, not from the title's answer
    expect(calls.some((c) => c.url === `${X}follow/by-tmdb?kind=series&tmdbId=77`)).toBe(true)
  })

  it('a followed series shows «Докачивать новые» on', async () => {
    await title(adult, '/title/series/77', (url) => (url.includes(`${X}follow/by-tmdb`) ? json(follow()) : undefined))
    expect(await screen.findByRole('button', { name: 'Докачивать новые', pressed: true })).toBeInTheDocument()
  })

  it('a server that cannot follow (no TMDB, starting) shows no «Следить»', async () => {
    await title(adult, '/title/series/77', (url) => (url.includes(`${X}follow/by-tmdb`) ? json({ error: 'Расширение запускается' }, 503) : undefined))
    await screen.findByRole('region', { name: 'Закреплённая раздача' })
    expect(screen.queryByRole('button', { name: /Докачивать новые/ })).not.toBeInTheDocument()
  })

  it('«Докачивать новые» asks the quality, the pin’s by default, and sends the one picked', async () => {
    const calls = await title(adult, '/title/series/77', (url, m) => {
      if (url.endsWith(`${X}pinned/series/77`) && m === 'GET') return json({ pin: pinOf() })
      if (url.endsWith(`${X}follow`) && m === 'POST') return json(follow({ resolution: '2160p' }))
      return undefined
    })
    await userEvent.click(await screen.findByRole('button', { name: 'Докачивать новые' }))
    const sheet = await screen.findByRole('dialog', { name: 'Докачивать новые серии' })
    await waitFor(() => expect(within(sheet).getByRole('radio', { name: '1080p' })).toHaveAttribute('aria-checked', 'true'))
    await userEvent.click(within(sheet).getByRole('radio', { name: '4K' }))
    await userEvent.click(within(sheet).getByRole('button', { name: 'Включить' }))
    await waitFor(() => expect(posts(calls, `${X}follow`)).toEqual([{ kind: 'series', tmdbId: 77, resolution: '2160p' }]))
    expect(await screen.findByRole('button', { name: 'Докачивать новые', pressed: true })).toBeInTheDocument()
    // the server checks a new follow by itself
    expect(calls.some((c) => c.url.includes('/check'))).toBe(false)
  })

  it('«Отмена» in the quality sheet follows nothing', async () => {
    const calls = await title(adult, '/title/series/77')
    await userEvent.click(await screen.findByRole('button', { name: 'Докачивать новые' }))
    const sheet = await screen.findByRole('dialog', { name: 'Докачивать новые серии' })
    expect(within(sheet).getByRole('radio', { name: 'Как в настройках' })).toHaveAttribute('aria-checked', 'true')
    await userEvent.click(within(sheet).getByRole('button', { name: 'Отмена' }))
    expect(posts(calls, `${X}follow`)).toEqual([])
  })

  it('a movie: «Скачать, когда выйдет» is on at once, without a quality', async () => {
    const calls = await title(adult, '/title/movie/42', (url, m) => (url.endsWith(`${X}follow`) && m === 'POST' ? json(follow({ kind: 'movie', tmdbId: 42 })) : undefined))
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать, когда выйдет' }))
    await waitFor(() => expect(posts(calls, `${X}follow`)).toEqual([{ kind: 'movie', tmdbId: 42 }]))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('stopping needs no confirmation and can be undone', async () => {
    const calls = await title(adult, '/title/series/77', (url) => (url.includes(`${X}follow/by-tmdb`) ? json(follow()) : undefined))
    await userEvent.click(await screen.findByRole('button', { name: 'Докачивать новые', pressed: true }))
    expect(screen.getByRole('button', { name: 'Докачивать новые' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Отменить' }))
    expect(screen.getByRole('button', { name: 'Докачивать новые', pressed: true })).toBeInTheDocument()
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false)
  })

  it('another profile’s follow: an adult sees «Следит: Мама» and cannot stop it; the admin can', async () => {
    const theirs = (url: string) => (url.includes(`${X}follow/by-tmdb`) ? json(follow({ addedBy: 'Мама', mine: false })) : undefined)
    const papa = { ...adult, id: 4, name: 'Папа' }
    const first = await title(papa, '/title/series/77', theirs)
    expect(first).toBeDefined()
    expect(await screen.findByText('Следит: Мама')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Докачивать новые/ })).not.toBeInTheDocument()
  })

  it('the admin may change another profile’s follow', async () => {
    await title(admin, '/title/series/77', (url) => (url.includes(`${X}follow/by-tmdb`) ? json(follow({ addedBy: 'Мама', mine: false })) : undefined))
    expect(await screen.findByRole('button', { name: 'Докачивать новые', pressed: true })).toBeInTheDocument()
  })

  it('a kid gets no panel and asks nothing of the addon', async () => {
    const calls = await title(kid, '/title/series/77')
    await new Promise((r) => setTimeout(r, 30))
    expect(screen.queryByRole('button', { name: /Докачивать новые/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Закреплённая раздача' })).not.toBeInTheDocument()
    expect(calls.some((c) => c.url.startsWith(X))).toBe(false)
  })
})

describe('«Добавить сезон…» on a library series (admins)', () => {
  async function library(user: { id: number }, answer?: Parameters<typeof serve>[1], t: object = show) {
    const calls = serve(user, answer, { title: t, item: libSeries })
    await signIn(user)
    renderAt('/item/10')
    await screen.findByRole('heading', { name: 'Дальний берег' })
    return calls
  }

  it('offers only the seasons that are not on the server and downloads the chosen one', async () => {
    const calls = await library(admin, (url, m) => (url.endsWith(`${X}acquire`) && m === 'POST' ? json({ infoHash: 'a'.repeat(40), fileIndex: 0, streamUrl: '', status: 'downloading' }) : undefined))
    // a library series: «Следить» too
    expect(await screen.findByRole('button', { name: 'Докачивать новые' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Добавить сезон…' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Загрузить на сервер' }))
    const sheet = await screen.findByRole('dialog', { name: 'Скачать на сервер' })
    // season 1 is on the server: only 2 (one season left: no «Все сезоны»)
    await userEvent.click(within(sheet).getByRole('button', { name: /^Сезон: / }))
    expect(screen.getAllByRole('menuitemradio').map((m) => m.textContent)).toEqual(['2'])
    await userEvent.keyboard('{Escape}')
    await userEvent.click(within(sheet).getByRole('button', { name: 'Скачать' }))
    await waitFor(() => expect(posts(calls, `${X}acquire`)).toEqual([{ tmdbId: 77, kind: 'series', mode: 'download', season: 2, releaseLink: undefined, quality: '1080p' }]))
  })

  it('says so when every season is on the server', async () => {
    await library(admin, undefined, { ...show, seasons: [{ number: 1, episodes: 1 }] })
    await userEvent.click(await screen.findByRole('button', { name: 'Добавить сезон…' }))
    expect(await screen.findByText('Все сезоны уже на сервере.')).toBeInTheDocument()
  })

  it('is not offered to an adult who is not the admin, nor to a kid', async () => {
    await library(adult)
    expect(await screen.findByRole('button', { name: 'Докачивать новые' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Добавить сезон…' })).not.toBeInTheDocument()
  })

  it('missingSeasons skips seasons with episodes on disk and specials', () => {
    const known = [{ number: 0, episodes: 2 }, { number: 1, episodes: 5 }, { number: 2, episodes: 5 }]
    expect(missingSeasons(known, [{ id: 1, number: 1, title: '', episodes: [{ id: 2 } as never] }, { id: 3, number: 2, title: '', episodes: [] }])).toEqual([{ number: 2, episodes: 5 }])
  })
})
