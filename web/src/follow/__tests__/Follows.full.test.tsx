import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import { listenEvent } from '@antresol/test-core/api/events'
import { session } from '@antresol/test-core/state/session'
import type { Follow } from '@antresol/test-addon/types'
import { FOLLOW_CHECKED, FOLLOW_QUEUED } from '../events'
import { Follows } from '../Follows'

// the loader asks for the addon's events by name once its bundle loaded
listenEvent(FOLLOW_CHECKED)
listenEvent(FOLLOW_QUEUED)

const admin = { id: 1, name: 'Александр', avatar: 'grad-1', hasPassword: true, isAdmin: true, isKid: false, maxRating: null }
const json = (b: unknown, status = 200) => new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

const hoursAgo = (h: number) => new Date(Date.now() - h * 3600_000).toISOString()
const shogun: Follow = {
  id: 7, kind: 'series', tmdbId: 126308, title: 'Сёгун', poster: '', active: true, lastChecked: hoursAgo(2), lastError: '', missing: 3, addedBy: 'Мама', mine: true,
}
const dune: Follow = {
  id: 8, kind: 'movie', tmdbId: 693134, title: 'Дюна: Часть вторая', poster: '', active: true, lastChecked: null, lastError: 'Нет связи с TMDB', missing: 0, addedBy: '', mine: false,
}

/** A stand-in for the one shared EventSource (jsdom has none). */
class FakeSource {
  static last: FakeSource | null = null
  listeners = new Map<string, ((m: MessageEvent) => void)[]>()
  onmessage: ((m: MessageEvent) => void) | null = null
  constructor() {
    FakeSource.last = this
  }
  addEventListener(type: string, fn: (m: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn])
  }
  close() {}
  emit(type: string, data: unknown) {
    for (const fn of this.listeners.get(type) ?? []) fn(new MessageEvent(type, { data: JSON.stringify(data) }))
  }
}

type Call = { url: string; method: string; body: unknown }
function stub(list: Follow[] = [shogun, dune], extra?: (url: string, init?: RequestInit) => Response | undefined) {
  const calls: Call[] = []
  vi.stubGlobal('EventSource', FakeSource)
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      const own = extra?.(url, init)
      if (own) return own
      if (url.endsWith('/api/login')) return json(admin)
      if (url.endsWith('/api/addons/torrents/x/follow') && method === 'GET') return json({ items: list })
      if (/\/x\/follow\/\d+\/check$/.test(url)) return json({ started: true }, 202)
      if (/\/x\/follow\/\d+$/.test(url) && method === 'PATCH') {
        const f = list.find((x) => url.endsWith(`/${x.id}`))!
        return json({ ...f, active: (JSON.parse(String(init!.body)) as { active: boolean }).active })
      }
      if (/\/x\/follow\/\d+$/.test(url) && method === 'DELETE') return json(undefined, 204)
      return json({})
    }),
  )
  return calls
}

function renderFollows() {
  return render(
    <MemoryRouter initialEntries={['/manage/follows']}>
      <Follows />
    </MemoryRouter>,
  )
}

describe('Слежение', () => {
  beforeEach(async () => {
    session.reset()
  })
  afterEach(() => vi.unstubAllGlobals())

  it('Follows lists items and check shows toast', async () => {
    const calls = stub()
    await session.login(1)
    renderFollows()
    const row = (await screen.findByText('Сёгун')).closest('li')!
    expect(within(row).getByText('Сериал')).toBeInTheDocument()
    expect(within(row).getByText(/Проверено 2 ч назад/)).toBeInTheDocument()
    expect(within(row).getByText('Не хватает 3 серий')).toBeInTheDocument()
    expect(within(row).getByText(/Добавила?: Мама/)).toBeInTheDocument()
    const movie = screen.getByText('Дюна: Часть вторая').closest('li')!
    expect(within(movie).getByText('Фильм')).toBeInTheDocument()
    expect(within(movie).getByText('Нет связи с TMDB')).toBeInTheDocument()

    await userEvent.click(within(row).getByRole('button', { name: 'Проверить «Сёгун»' }))
    await waitFor(() => expect(calls.some((c) => c.url.endsWith('/api/addons/torrents/x/follow/7/check') && c.method === 'POST')).toBe(true))
    // the result comes later: what it queued (admins), then follow-checked, which names only the follow
    act(() => FakeSource.last!.emit(FOLLOW_QUEUED, { followId: 7, queued: ['S01E08', 'S01E09'] }))
    act(() => FakeSource.last!.emit(FOLLOW_CHECKED, { followId: 7 }))
    expect(await screen.findByText('Поставлено в загрузку: S01E08, S01E09')).toBeInTheDocument()
    // the design system's Toast (above the tab bar on a phone), not a page-made capsule
    expect(screen.getByText('Поставлено в загрузку: S01E08, S01E09').closest('[data-testid="toast-region"]')).not.toBeNull()

    await userEvent.click(within(movie).getByRole('button', { name: 'Проверить «Дюна: Часть вторая»' }))
    act(() => FakeSource.last!.emit(FOLLOW_CHECKED, { followId: 8 }))
    // the follow read again says how it went: its lastError
    expect(await screen.findByText('Нет связи с TMDB', { selector: '[data-testid="toast-region"] *' })).toBeInTheDocument()
  })

  it('a check that queued nothing and found everything says so from the follow it reads again', async () => {
    const done = { ...shogun, missing: 0, lastChecked: hoursAgo(0) }
    stub([shogun])
    await session.login(1)
    renderFollows()
    const row = (await screen.findByText('Сёгун')).closest('li')!
    await userEvent.click(within(row).getByRole('button', { name: 'Проверить «Сёгун»' }))
    // the follow the check left: nothing missing
    vi.mocked(fetch).mockImplementation(async (input) => (String(input).endsWith('/x/follow') ? json({ items: [done] }) : json({})))
    act(() => FakeSource.last!.emit(FOLLOW_CHECKED, { followId: 7 }))
    expect(await screen.findByText('Всё уже есть')).toBeInTheDocument()
  })

  it('unfollow asks confirmation', async () => {
    const calls = stub()
    await session.login(1)
    renderFollows()
    const row = (await screen.findByText('Сёгун')).closest('li')!
    await userEvent.click(within(row).getByRole('button', { name: 'Не следить за «Сёгун»' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('Не следить за «Сёгун»?')).toBeInTheDocument()
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Не следить' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/api/addons/torrents/x/follow/7'))).toBe(true))
    await waitFor(() => expect(screen.queryByText('Сёгун')).not.toBeInTheDocument())
  })

  it('Активно toggle sends PATCH active false', async () => {
    const calls = stub()
    await session.login(1)
    renderFollows()
    const row = (await screen.findByText('Сёгун')).closest('li')!
    const toggle = within(row).getByRole('switch', { name: 'Активно: Сёгун' })
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    await userEvent.click(toggle)
    await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')).toMatchObject({ url: expect.stringMatching(/\/x\/follow\/7$/), body: { active: false } }))
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'false'))
    // an inactive follow is not checked
    expect(within(row).getByRole('button', { name: 'Проверить «Сёгун»' })).toBeDisabled()
  })
})

describe('Мои слежения', () => {
  beforeEach(() => session.reset())
  afterEach(() => vi.unstubAllGlobals())

  it('an adult sees only their own follows and can only unfollow them', async () => {
    const mama = { id: 2, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
    const calls = stub([shogun, dune], (url) => (url.endsWith('/api/login') ? json(mama) : undefined))
    await session.login(2)
    render(
      <MemoryRouter initialEntries={['/follows']}>
        <Follows mine />
      </MemoryRouter>,
    )
    expect(await screen.findByRole('heading', { name: 'Мои слежения' })).toBeInTheDocument()
    const row = screen.getByText('Сёгун').closest('li')!
    expect(screen.queryByText('Дюна: Часть вторая')).not.toBeInTheDocument()
    expect(within(row).queryByRole('switch')).not.toBeInTheDocument()
    expect(within(row).queryByRole('button', { name: /Проверить/ })).not.toBeInTheDocument()
    await userEvent.click(within(row).getByRole('button', { name: 'Не следить за «Сёгун»' }))
    await userEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Не следить' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/api/addons/torrents/x/follow/7'))).toBe(true))
  })
})

describe('Мои слежения by profile id', () => {
  beforeEach(() => session.reset())
  afterEach(() => vi.unstubAllGlobals())

  it('the admin’s own list goes by id, not by a shared name', async () => {
    const namesake = { ...admin, name: 'Мама' }
    // the admin is named like the author of «Сёгун» but did not add it; «Дюна» is the admin’s
    stub([{ ...shogun, mine: false }, { ...dune, addedBy: 'Мама', mine: true }], (url) => (url.endsWith('/api/login') ? json(namesake) : undefined))
    await session.login(1)
    render(
      <MemoryRouter initialEntries={['/follows']}>
        <Follows mine />
      </MemoryRouter>,
    )
    expect(await screen.findByText('Дюна: Часть вторая')).toBeInTheDocument()
    expect(screen.queryByText('Сёгун')).not.toBeInTheDocument()
  })
})

describe('Follows in English', () => {
  it('says what is missing in the chosen language', async () => {
    const { setLanguagePref } = await import('@antresol/test-core/i18n')
    const { missingText, checkedText } = await import('../Follows')
    setLanguagePref('en')
    expect(missingText(shogun)).toBe('3 episodes missing')
    expect(missingText({ ...shogun, missing: 1 })).toBe('1 episode missing')
    expect(checkedText({ queued: [], missing: 0, lastError: '' })).toBe('You already have everything')
    setLanguagePref('ru')
    expect(missingText(shogun)).toBe('Не хватает 3 серий')
  })
})
