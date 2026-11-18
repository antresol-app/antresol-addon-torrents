// The «Смотреть ▾» of a «Найти и добавить» row: the addon's own way, then the
// watch ways of the profile's other addons (the SDK's useWatchWays, drawn
// by the app's AddonWaysOutlet). The row never lists the addon's own ways
// twice.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import { registerAddon, resetAddonsForTests } from '@antresol/test-registry'
import { AddonWaysOutlet } from '@antresol/test-core/addons/watchWays'
import { session } from '@antresol/test-core/state/session'
import { FindAndAdd } from '../find/FindAndAdd'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const adult = { id: 2, name: 'Маша', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }

function serve() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(adult)
      if (url.includes('/x/discover'))
        return json([
          { tmdbId: 1, kind: 'movie', title: 'Дюна', year: 2021, poster: '/p.jpg', overview: '', inLibrary: false, libraryItemId: null },
          { tmdbId: 2, kind: 'series', title: 'Сёгун', year: 2024, poster: '', overview: '', inLibrary: false, libraryItemId: null },
        ])
      return json({})
    }),
  )
}

const renderRows = (path = '/search?q=Дюна') =>
  render(
    <MemoryRouter initialEntries={[path]}>
      <FindAndAdd text="Дюна" q="Дюна" scope="all" nothing={false} admin={false} />
      <AddonWaysOutlet />
    </MemoryRouter>,
  )

const cardOf = async (title: string) => (await screen.findAllByRole('link', { name: title }, { timeout: 3000 }))[0]!.closest('li')!
const radios = () => screen.getAllByRole('menuitemradio').map((m) => m.textContent)

describe('a row’s «Смотреть ▾»', () => {
  beforeEach(async () => {
    serve()
    await session.login(2)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    session.reset()
    resetAddonsForTests()
  })

  it('a row offers another addon’s watch way', async () => {
    const ran: string[] = []
    registerAddon('beta', {
      strings: { ru: { way: 'Способ Б' }, en: {} },
      itemActions: [
        { label: 'way', icon: 'play', watchWay: true, run: (item) => void ran.push(`${item.kind}:${item.tmdbId}:${item.title}`) },
        // a button, not a way
        { label: 'Кнопка', icon: 'download', run: () => undefined },
      ],
    })
    renderRows()
    const card = await cardOf('Дюна')
    await userEvent.click(within(card).getByRole('button', { name: 'Другие способы просмотра' }))
    // the addon's own way first, the other addon's after it
    await waitFor(() => expect(radios()).toEqual(['Торрент', 'Способ Б']))
    await userEvent.click(screen.getByRole('menuitemradio', { name: 'Способ Б' }))
    expect(ran).toEqual(['movie:1:Дюна'])
    // the main part still runs the addon's own way (no stream started by the other one)
    expect(within(card).getByRole('button', { name: 'Смотреть' })).toBeInTheDocument()
    // no download was started by choosing it
    expect(vi.mocked(fetch).mock.calls.some(([u]) => String(u).endsWith('/x/acquire'))).toBe(false)
  })

  it('the addon’s own ways are not listed a second time', async () => {
    registerAddon('torrents', { itemActions: [{ label: 'Смотреть сейчас', icon: 'play', watchWay: true, run: () => undefined }] })
    registerAddon('beta', { strings: { ru: { way: 'Способ Б' }, en: {} }, itemActions: [{ label: 'way', icon: 'play', watchWay: true, run: () => undefined }] })
    renderRows()
    await userEvent.click(within(await cardOf('Дюна')).getByRole('button', { name: 'Другие способы просмотра' }))
    await waitFor(() => expect(radios()).toEqual(['Торрент', 'Способ Б']))
  })

  it('a way whose useAvailable still asks joins the menu when it answers, a series row asks none', async () => {
    const asked: unknown[] = []
    let answer: boolean | null = null
    const listeners = new Set<() => void>()
    const { useSyncExternalStore } = await import('react')
    registerAddon('beta', {
      strings: { ru: { way: 'Способ Б' }, en: {} },
      itemActions: [
        {
          label: 'way',
          icon: 'play',
          watchWay: true,
          useAvailable: (t) => (
            asked.push(t),
            useSyncExternalStore(
              (l) => (listeners.add(l), () => void listeners.delete(l)),
              () => answer,
            )
          ),
          run: () => undefined,
        },
      ],
    })
    renderRows()
    const card = await cardOf('Дюна')
    await userEvent.click(within(card).getByRole('button', { name: 'Другие способы просмотра' }))
    // still asking: the menu holds the release picker alone, no way to choose from
    expect(screen.getByRole('menuitem', { name: 'Выбрать раздачу…' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitemradio')).not.toBeInTheDocument()
    answer = true
    act(() => listeners.forEach((l) => l()))
    await waitFor(() => expect(radios()).toEqual(['Торрент', 'Способ Б']))
    // only the film row asked: a series row has no «Смотреть ▾»
    expect(asked.length).toBeGreaterThan(0)
    expect(asked.every((t) => (t as { kind: string; tmdbId: number }).kind === 'movie' && (t as { tmdbId: number }).tmdbId === 1)).toBe(true)
  })

  it('?way= on the search page runs no way of any row', async () => {
    const ran: string[] = []
    registerAddon('beta', {
      strings: { ru: { way: 'Способ Б' }, en: {} },
      itemActions: [{ label: 'way', icon: 'play', watchWay: true, run: (item) => void ran.push(item.title) }],
    })
    renderRows('/search?q=Дюна&way=beta:way')
    await userEvent.click(within(await cardOf('Дюна')).getByRole('button', { name: 'Другие способы просмотра' }))
    await waitFor(() => expect(radios()).toEqual(['Торрент', 'Способ Б']))
    expect(ran).toEqual([])
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})
