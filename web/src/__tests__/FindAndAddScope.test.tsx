// «Найти и добавить» under «Сериалы» / «Фильмы»: the filter narrows the
// catalog's answer, but the other kind is not hidden — a few of it under
// «Среди фильмов» («The Gentlemen»: the film beside the series), the rest
// behind «Показать все», which switches the page's filter. A misspelt query
// (approx candidates) says the titles are similar ones.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router'
import { resetAddonsForTests } from '@antresol/test-registry'
import { session } from '@antresol/test-core/state/session'
import { FindAndAdd, type SearchScope } from '../find/FindAndAdd'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const adult = { id: 2, name: 'Маша', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
const cand = (tmdbId: number, kind: string, title: string, year: number, approx = false) => ({
  tmdbId,
  kind,
  title,
  year,
  poster: '',
  overview: '',
  inLibrary: false,
  libraryItemId: null,
  ...(approx ? { approx } : {}),
})

let answer: unknown[] = []
function serve() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(adult)
      if (url.includes('/x/discover')) return json(answer)
      return json({})
    }),
  )
}

function Where() {
  const l = useLocation()
  return <output data-testid="where">{l.search}</output>
}

const renderAt = (q: string, scope: SearchScope) =>
  render(
    <MemoryRouter initialEntries={[`/search?q=${encodeURIComponent(q)}&scope=${scope}`]}>
      <FindAndAdd text={q} q={q} scope={scope} nothing admin={false} />
      <Where />
    </MemoryRouter>,
  )

describe('«Найти и добавить» under a films-or-series filter', () => {
  beforeEach(async () => {
    serve()
    await session.login(2)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    session.reset()
    resetAddonsForTests()
  })

  it('«Сериалы» still offers the film of the same name', async () => {
    answer = [cand(522627, 'movie', 'The Gentlemen', 2020), cand(236235, 'series', 'The Gentlemen', 2024), cand(2349, 'series', 'The League of Gentlemen', 1999)]
    renderAt('The Gentlemen', 'series')
    const heading = await screen.findByRole('heading', { name: 'Среди фильмов' }, { timeout: 3000 })
    const group = heading.closest('div')!.parentElement!
    expect(within(group).getAllByRole('link', { name: 'The Gentlemen' }).length).toBeGreaterThan(0)
    // the series first, the film under its own heading after them
    const lists = screen.getAllByRole('list')
    expect(within(lists[0]!).getAllByText(/Сериал/).length).toBe(2)
    // only three: no «Показать все» for one
    expect(screen.queryByRole('button', { name: /Показать все/ })).toBeNull()
  })

  it('«Показать все» switches the filter to the other kind', async () => {
    answer = [1, 2, 3, 4].map((i) => cand(i, 'movie', `Фильм ${i}`, 2000 + i)).concat([cand(9, 'series', 'Сериал', 2020)])
    renderAt('Фильм', 'series')
    await userEvent.click(await screen.findByRole('button', { name: 'Показать все (4)' }, { timeout: 3000 }))
    expect(screen.getByTestId('where').textContent).toContain('scope=movies')
  })

  it('a misspelt query says the titles are similar ones', async () => {
    answer = [cand(522627, 'movie', 'Джентльмены', 2020, true), cand(236235, 'series', 'Джентльмены', 2024, true)]
    renderAt('Джентельмены', 'all')
    expect(await screen.findByText('Точно так ничего не называется. Похожие названия:', undefined, { timeout: 3000 })).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: 'Джентльмены' }).length).toBeGreaterThan(0)
  })

  it('an exact answer has no such note', async () => {
    answer = [cand(522627, 'movie', 'The Gentlemen', 2020)]
    renderAt('The Gentlemen', 'all')
    await screen.findAllByRole('link', { name: 'The Gentlemen' }, { timeout: 3000 })
    expect(screen.queryByText(/Похожие названия/)).toBeNull()
  })
})
