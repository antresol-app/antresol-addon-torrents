// Test helpers (no cases): the core's item and title page with the addon
// registered (its itemPanels, itemActions and episodeActions draw into it),
// and a fake server for the core's and the addon's routes.
import { render } from '@testing-library/react'
import { vi } from 'vitest'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import { ItemDetail } from '@antresol/test-core/pages/ItemDetail'
import { session } from '@antresol/test-core/state/session'

export const json = (b: unknown, status = 200) =>
  new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

export const adult = { id: 2, name: 'Мама', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
export const online = { ...adult, canWatchOnline: true }
export const admin = { ...adult, id: 1, name: 'Папа', isAdmin: true }
export const kid = { ...adult, id: 3, name: 'Коля', isKid: true }

export const X = '/api/addons/torrents/x/'

export type Call = { url: string; method: string; body: unknown }
export type Answer = (url: string, method: string, body: unknown) => Response | undefined

/**
 * The fake server: `answer` first, then the user, the title, an empty
 * guide, no follow (404), no pin, no download; everything else `{}`.
 */
export function serve(user: object, answer: Answer = () => undefined, extra: { title?: object; item?: object } = {}) {
  const calls: Call[] = []
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = init?.method ?? 'GET'
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body
    calls.push({ url, method, body })
    const own = answer(url, method, body)
    if (own) return own
    if (url.endsWith('/api/login')) return json(user)
    if (extra.title && /\/api\/title\/(movie|series)\/\d+/.test(url) && !url.includes('/episodes') && !url.includes('/choice')) return json(extra.title)
    if (extra.item && /\/api\/items\/\d+$/.test(url)) return json(extra.item)
    if (url.endsWith('/episodes')) return json({ error: 'nope' }, 404)
    if (url.includes(`${X}follow/by-tmdb`)) return json({ error: 'Не найдено' }, 404)
    if (url.includes(`${X}pinned/`) && method === 'GET') return json({ pin: null })
    if (url.includes(`${X}acquire/progress`)) return new Response(null, { status: 204 })
    if (url.includes(`${X}releases`)) return json({ releases: [], rejected: [], failed: [] })
    if (url.includes('/api/online/sources')) return json({ sources: [] })
    return json({})
  })
  vi.stubGlobal('fetch', f)
  return calls
}

/** The POST bodies sent to `path` (an addon path without its x/ prefix, or a core one). */
export const posts = (calls: Call[], path: string) =>
  calls.filter((c) => c.method === 'POST' && c.url.split('?')[0]!.endsWith(path)).map((c) => c.body)

const Where = () => {
  const l = useLocation()
  return (
    <output data-testid="where" data-state={JSON.stringify(l.state ?? null)}>
      {l.pathname}
      {l.search}
    </output>
  )
}

export function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/item/:id" element={<ItemDetail />} />
        <Route path="/title/:kind/:tmdbId" element={<ItemDetail />} />
        <Route path="*" element={null} />
      </Routes>
      <Where />
    </MemoryRouter>,
  )
}

export async function signIn(user: { id: number }) {
  session.reset()
  await session.login(user.id)
}

/** The addon's bundle, once (its module registers when it first loads). */
export const registerTheAddon = () => import('@antresol/test-addon/index')
