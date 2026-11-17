// A title that is not on the server, on the core's page with the addon:
// «Смотреть сейчас» is a way of the core's «Смотреть ▾» (remembered per
// profile), «Скачать» a round button whose sheet asks the quality and opens
// the release table; a running download's ring, menu and stall note
// are the addon's panel. Rewired from the old page's cases (the
// season pickers of the page moved into the sheets; also the cases of the
// old series and episode guide pages).
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { resetAddonsForTests } from '@antresol/test-registry'
import { session } from '@antresol/test-core/state/session'
import type { Release } from '@antresol/test-addon/types'
import { WATCH_WAY } from '@antresol/test-addon/episodes/useEpisodePlay'
import { adult, admin, json, kid, online, posts, registerTheAddon, renderAt, serve, signIn, X, type Call } from './page'

const IH = 'a'.repeat(40)
const movie = { tmdbId: 42, kind: 'movie', title: 'Дюна', year: 2021, poster: '', backdrop: '', overview: '', inLibrary: false, libraryItemId: null, genres: [] }
const show = {
  tmdbId: 77, kind: 'series', title: 'Сёгун', year: 2024, poster: '', backdrop: '', overview: '', inLibrary: false,
  libraryItemId: null, genres: [], seasons: [{ number: 1, episodes: 10 }, { number: 2, episodes: 8 }],
}
const rel: Release = {
  title: 'Dune.2021.2160p.WEB-DL', link: 'https://t/dune', size: 20 << 30, seeders: 30, leechers: 0, sourceId: 5,
  sourceName: 'RuTracker', score: 90, reasons: [], resolution: 2160,
}

let progress: { status: string; done: number; size: number; stalled?: string } | null = null
let pin: object | null = null

function addon(url: string, method: string, body: unknown): Response | undefined {
  if (url.includes(`${X}releases`)) return json({ releases: [rel], rejected: [], failed: [] })
  // the batch form too (titles asked in the same moment)
  if (url.includes(`${X}acquire/progress/batch`)) {
    const keys = new URL(url, 'http://x').searchParams.getAll('t')
    return json(progress ? Object.fromEntries(keys.map((k) => [k, progress])) : {})
  }
  if (url.includes(`${X}acquire/progress`)) return progress ? json(progress) : new Response(null, { status: 204 })
  if (url.endsWith(`${X}acquire/cancel`)) {
    progress = null
    return json({ cancelled: 1 })
  }
  if (url.endsWith(`${X}acquire`) && method === 'POST') {
    if ((body as { mode: string }).mode === 'download') progress = { status: 'active', done: 50, size: 100 }
    return json({ infoHash: IH, fileIndex: 0, streamUrl: '', status: 'downloading' })
  }
  if (url.includes(`${X}watch/${IH}/`)) return json({ inputId: 'tor-in' })
  if (/\/x\/pinned\/(movie|series)\/\d+$/.test(url) && method === 'GET') return json({ pin })
  if (/\/x\/pinned\/(movie|series)\/\d+\/play$/.test(url)) return json({ inputId: 'pin-in' })
  return undefined
}
const acquires = (calls: Call[]) => posts(calls, `${X}acquire`)

async function open(user: { id: number }, path = '/title/movie/42') {
  const calls = serve(user, addon, { title: path.includes('series') ? show : movie })
  await signIn(user)
  const view = renderAt(path)
  await screen.findByRole('heading', { name: path.includes('series') ? 'Сёгун' : 'Дюна' })
  return { calls, view }
}
const where = () => screen.getByTestId('where')

beforeAll(registerTheAddon)
afterAll(() => resetAddonsForTests())
beforeEach(() => {
  localStorage.clear()
  progress = null
  pin = null
})
afterEach(() => {
  vi.unstubAllGlobals()
  session.reset()
})

describe('«Смотреть сейчас» in the core’s «Смотреть ▾»', () => {
  it('an online profile gets «Смотреть сейчас» beside «Онлайн»; choosing it streams and is remembered', async () => {
    const { calls, view } = await open(online)
    await userEvent.click(await screen.findByRole('button', { name: 'Другие способы просмотра' }))
    expect(screen.getAllByRole('menuitemradio').map((m) => m.textContent)).toEqual(['Онлайн', 'Смотреть сейчас'])
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'Смотреть сейчас' }))
    // a movie streams at once: the server's pick, then an input of the core's player
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/tor-in'))
    expect(acquires(calls)).toEqual([{ tmdbId: 42, kind: 'movie', mode: 'stream' }])
    expect(localStorage.getItem('antresol.watchWay.2')).toBe(WATCH_WAY)
    view.unmount()

    renderAt('/title/movie/42')
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(acquires(calls)).toHaveLength(2))
  })

  it('without online «Смотреть» is the addon’s way alone', async () => {
    const { calls } = await open(adult)
    expect(await screen.findByRole('button', { name: 'Смотреть' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Другие способы просмотра' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(acquires(calls)).toEqual([{ tmdbId: 42, kind: 'movie', mode: 'stream' }]))
  })

  it('with a pin a movie streams from it, never through x/acquire', async () => {
    pin = { sourceId: '5', link: 'https://t/dune', details: '', releaseTitle: 'Dune.2021.1080p', infoHash: IH, quality: '1080p', episodes: 0, pinnedAt: '', lastChecked: null, lastError: '', suggestion: null }
    const { calls } = await open(adult)
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/pin-in'))
    expect(posts(calls, `${X}pinned/movie/42/play`)).toEqual([{}])
    expect(acquires(calls)).toEqual([])
  })

  it('a series asks the episode (a «Вышло новое» card’s preselected) and plays it from the pin', async () => {
    pin = { sourceId: '5', link: 'https://t/s', details: '', releaseTitle: 'Shogun.S02', infoHash: IH, quality: '1080p', episodes: 8, pinnedAt: '', lastChecked: null, lastError: '', suggestion: null }
    const { calls } = await open(adult, '/title/series/77?season=2&episode=8')
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть' }))
    const sheet = await screen.findByRole('dialog', { name: 'Смотреть сейчас' })
    expect(within(sheet).getByRole('button', { name: /^Сезон: 2/ })).toBeInTheDocument()
    expect(within(sheet).getByRole('button', { name: /^Серия: 8/ })).toBeInTheDocument()
    await userEvent.click(within(sheet).getByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/pin-in'))
    expect(posts(calls, `${X}pinned/series/77/play`)).toEqual([{ season: 2, episode: 8 }])
  })

  it('«Выбрать раздачу…» streams the release picked', async () => {
    const { calls } = await open(adult, '/title/series/77')
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть' }))
    const ask = await screen.findByRole('dialog', { name: 'Смотреть сейчас' })
    await userEvent.click(within(ask).getByRole('button', { name: 'Выбрать раздачу…' }))
    const sheet = await screen.findByRole('dialog', { name: /Раздачи: Сёгун/ })
    const row = (await within(sheet).findAllByRole('listitem'))[0]!
    await userEvent.click(within(row).getByRole('button', { name: 'Смотреть' }))
    await waitFor(() => expect(acquires(calls)).toEqual([{ tmdbId: 77, kind: 'series', mode: 'stream', season: 1, episode: 1, releaseLink: 'https://t/dune', sourceId: 5 }]))
  })

  it('a kid gets neither the way nor «Скачать»', async () => {
    await open(kid)
    await new Promise((r) => setTimeout(r, 30))
    expect(screen.queryByRole('button', { name: 'Смотреть' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Скачать' })).not.toBeInTheDocument()
  })
})

describe('«Скачать» and the ring of a running download', () => {
  it('asks the quality first and sends it', async () => {
    const { calls } = await open(adult)
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать' }))
    const sheet = await screen.findByRole('dialog', { name: 'Скачать на сервер' })
    // 4K once the search has it; 1080p preselected all the same
    await waitFor(() => expect(within(sheet).getByRole('radio', { name: '4K' })).toBeInTheDocument())
    expect(within(sheet).getByRole('radio', { name: '1080p' })).toHaveAttribute('aria-checked', 'true')
    await userEvent.click(within(sheet).getByRole('button', { name: 'Скачать' }))
    await waitFor(() => expect(acquires(calls)).toEqual([{ tmdbId: 42, kind: 'movie', mode: 'download', releaseLink: undefined, quality: '1080p' }]))
    // the panel's ring follows the download it started
    await waitFor(() => expect(screen.getByRole('progressbar', { name: 'Прогресс загрузки' })).toHaveAttribute('aria-valuenow', '50'))
  })

  it('a 4K-only search neither preselects 4K nor hides 1080p', async () => {
    await open(adult)
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать' }))
    const sheet = await screen.findByRole('dialog', { name: 'Скачать на сервер' })
    await waitFor(() => expect(within(sheet).getAllByRole('radio').map((r) => r.textContent)).toEqual(['720p', '1080p', '4K']))
    expect(within(sheet).getByRole('radio', { name: '4K' })).toHaveAttribute('aria-checked', 'false')
  })

  it('«Выбрать раздачу…»: the best release first keeps the quality asked; «Закрепить» and «Загрузить» per row', async () => {
    const { calls } = await open(adult)
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать' }))
    await userEvent.click(within(await screen.findByRole('dialog', { name: 'Скачать на сервер' })).getByRole('button', { name: 'Выбрать раздачу…' }))
    const sheet = await screen.findByRole('dialog', { name: /Раздачи: Дюна/ })
    const row = (await within(sheet).findAllByRole('listitem'))[0]!
    expect(within(row).getAllByRole('button').map((b) => b.textContent)).toEqual(['Закрепить', 'Загрузить'])
    const best = within(sheet).getByText('Лучшая раздача — выберет Antresol').closest('[data-best]') as HTMLElement
    await userEvent.click(within(best).getByRole('button', { name: 'Загрузить' }))
    await waitFor(() => expect(acquires(calls)).toEqual([{ tmdbId: 42, kind: 'movie', mode: 'download', quality: '1080p' }]))
  })

  it('«Закрепить» on a release row pins it with the title’s name and poster', async () => {
    const { calls } = await open(adult, '/title/series/77')
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать' }))
    await userEvent.click(within(await screen.findByRole('dialog', { name: 'Скачать на сервер' })).getByRole('button', { name: 'Выбрать раздачу…' }))
    const sheet = await screen.findByRole('dialog', { name: /Раздачи: Сёгун/ })
    await userEvent.click(await within(sheet).findByRole('button', { name: 'Закрепить «Dune.2021.2160p.WEB-DL»' }))
    await waitFor(() =>
      expect(posts(calls, `${X}pinned/series/77`)).toEqual([{ sourceId: 5, link: 'https://t/dune', releaseTitle: 'Dune.2021.2160p.WEB-DL', name: 'Сёгун', poster: '' }]),
    )
  })

  it('a series: the chosen season, or «Все сезоны» (one download per season)', async () => {
    const { calls } = await open(adult, '/title/series/77')
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать' }))
    let sheet = await screen.findByRole('dialog', { name: 'Скачать на сервер' })
    await userEvent.click(within(sheet).getByRole('button', { name: /^Сезон: / }))
    await userEvent.click(screen.getByRole('menuitemradio', { name: '2' }))
    await userEvent.click(within(sheet).getByRole('button', { name: 'Скачать' }))
    await waitFor(() => expect(acquires(calls).map((b) => (b as { season?: number }).season)).toEqual([2]))
    await userEvent.click(within(await screen.findByRole('dialog', { name: 'Скачать на сервер' })).getByRole('button', { name: 'Готово' }))

    await userEvent.click(screen.getByRole('button', { name: 'Скачать' }))
    sheet = await screen.findByRole('dialog', { name: 'Скачать на сервер' })
    await userEvent.click(within(sheet).getByRole('button', { name: /^Сезон: / }))
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'Все сезоны' }))
    await userEvent.click(within(sheet).getByRole('button', { name: 'Скачать' }))
    await waitFor(() => expect(acquires(calls).map((b) => (b as { season?: number }).season)).toEqual([2, 1, 2]))
  })

  it('a running download: the ring is a menu, «Отменить загрузку» asks first and stops it', async () => {
    progress = { status: 'active', done: 30, size: 100 }
    const { calls } = await open(adult)
    await userEvent.click(await screen.findByRole('button', { name: /^Загружается, 30/ }))
    expect(screen.getAllByRole('menuitem').map((m) => m.textContent)).toEqual(['Выбрать другую раздачу…', 'Отменить загрузку'])
    await userEvent.click(screen.getByRole('menuitem', { name: 'Отменить загрузку' }))
    const ask = await screen.findByRole('alertdialog', { name: 'Отменить загрузку?' })
    await userEvent.click(within(ask).getByRole('button', { name: 'Отменить загрузку' }))
    await waitFor(() => expect(posts(calls, `${X}acquire/cancel`)).toEqual([{ kind: 'movie', tmdbId: 42, season: 0 }]))
    await waitFor(() => expect(screen.queryByRole('progressbar', { name: 'Прогресс загрузки' })).not.toBeInTheDocument())
  })

  it('«Выбрать другую раздачу…» downloads the picked release in place of the running one', async () => {
    progress = { status: 'paused', done: 30, size: 100 }
    const { calls } = await open(adult)
    await userEvent.click(await screen.findByRole('button', { name: /^Пауза, 30/ }))
    await userEvent.click(screen.getByRole('menuitem', { name: 'Выбрать другую раздачу…' }))
    const sheet = await screen.findByRole('dialog', { name: /Раздачи: Дюна/ })
    expect(within(sheet).getByText('Текущая загрузка будет отменена, когда начнётся новая.')).toBeInTheDocument()
    const row = (await within(sheet).findAllByRole('listitem'))[0]!
    await userEvent.click(within(row).getByRole('button', { name: 'Загрузить' }))
    await waitFor(() => expect(acquires(calls)).toEqual([{ tmdbId: 42, kind: 'movie', mode: 'download', releaseLink: 'https://t/dune', sourceId: 5, replace: true }]))
  })

  it('a stalled download says why and offers another release; one without a hint says nothing', async () => {
    progress = { status: 'active', done: 0, size: 100, stalled: 'no_peers' }
    const { calls, view } = await open(adult)
    const note = await screen.findByTestId('stalled-note')
    expect(within(note).getByRole('status')).toHaveTextContent('Нет участников раздачи — ждём…')
    await userEvent.click(within(note).getByRole('button', { name: 'Выбрать другую раздачу…' }))
    const sheet = await screen.findByRole('dialog', { name: /Раздачи: Дюна/ })
    const row = (await within(sheet).findAllByRole('listitem'))[0]!
    await userEvent.click(within(row).getByRole('button', { name: 'Загрузить' }))
    await waitFor(() => expect(acquires(calls)).toEqual([expect.objectContaining({ releaseLink: 'https://t/dune', replace: true })]))
    view.unmount()

    progress = { status: 'active', done: 30, size: 100 }
    renderAt('/title/movie/42')
    await screen.findByRole('button', { name: /^Загружается, 30/ })
    expect(screen.queryByTestId('stalled-note')).not.toBeInTheDocument()
  })

  it('?replace=1&season=2 (from «Загрузки») opens the release sheet for that season, replacing', async () => {
    progress = { status: 'active', done: 0, size: 100, stalled: 'no_peers' }
    const { calls } = await open(adult, '/title/series/77?replace=1&season=2')
    const sheet = await screen.findByRole('dialog', { name: /Раздачи: Сёгун, сезон 2/ })
    expect(within(sheet).getByText('Текущая загрузка будет отменена, когда начнётся новая.')).toBeInTheDocument()
    const rows = await within(sheet).findAllByRole('listitem')
    await userEvent.click(within(rows[rows.length - 1]!).getByRole('button', { name: 'Загрузить' }))
    await waitFor(() => expect(acquires(calls)).toEqual([expect.objectContaining({ season: 2, replace: true })]))
    expect(where()).not.toHaveTextContent('replace')
  })

  it('?replace=1&season=all (a whole-series pack) opens «Все сезоны» and replaces the pack', async () => {
    progress = { status: 'active', done: 0, size: 100, stalled: 'no_peers' }
    const { calls } = await open(adult, '/title/series/77?replace=1&season=all')
    const sheet = await screen.findByRole('dialog', { name: /все сезоны/i })
    const releases = calls.map((c) => c.url).filter((u) => u.includes(`${X}releases`))
    expect(releases.length).toBeGreaterThan(0)
    expect(releases.every((u) => !u.includes('season='))).toBe(true)
    const rows = await within(sheet).findAllByRole('listitem')
    await userEvent.click(within(rows[rows.length - 1]!).getByRole('button', { name: 'Загрузить' }))
    await waitFor(() => expect(acquires(calls)).toHaveLength(1))
    expect(acquires(calls)[0]).toEqual(expect.objectContaining({ replace: true }))
    expect((acquires(calls)[0] as { season?: number }).season ?? 0).toBe(0)
  })

  it('the admin may pick a rejected release too (the release table’s admin rows)', async () => {
    await open(admin)
    await userEvent.click(await screen.findByRole('button', { name: 'Скачать' }))
    expect(await screen.findByRole('dialog', { name: 'Скачать на сервер' })).toBeInTheDocument()
  })
})
