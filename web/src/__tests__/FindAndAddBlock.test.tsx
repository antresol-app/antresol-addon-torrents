// The search block «Поиск» draws under its results (searchBlocks): the
// signed-in profile decides `admin`, «Смотреть» opens the core's input
// player, and a row offers no online way.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation, useParams } from 'react-router'

let me: { id: number; isAdmin: boolean; isKid: boolean; lang: 'ru' } | null = null
vi.mock('@antresol/addon-sdk', async (orig) => ({
  ...(await orig<typeof import('@antresol/addon-sdk')>()),
  useUser: () => me,
}))

import { AddonWaysOutlet } from '@antresol/test-core/addons/watchWays'
import { FindAndAddBlock } from '../find/FindAndAddBlock'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const IH = 'a'.repeat(40)

function stub() {
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/api/addons/torrents/x/discover'))
      return json([{ tmdbId: 1, kind: 'movie', title: 'Дюна', year: 2021, poster: 'p.jpg', overview: '', inLibrary: false, libraryItemId: null }])
    if (url.endsWith('/api/addons/torrents/x/acquire') && init?.method === 'POST') return json({ infoHash: IH, fileIndex: 2, streamUrl: '', status: 'downloading' })
    if (url.endsWith(`/api/addons/torrents/x/watch/${IH}/2`) && init?.method === 'POST') return json({ inputId: 'in-7' })
    if (url.includes('/api/addons/torrents/x/acquire/progress')) return new Response(null, { status: 204 })
    return json({ error: 'нет' }, 404)
  })
  vi.stubGlobal('fetch', f)
  return f
}

function Player() {
  const { id } = useParams()
  const st = useLocation().state as { title?: string; poster?: string } | null
  return (
    <h1>
      Плеер {id}: {st?.title} / {st?.poster}
    </h1>
  )
}

const renderBlock = () =>
  render(
    <MemoryRouter initialEntries={['/search?q=Дюна']}>
      <Routes>
        <Route path="/search" element={<FindAndAddBlock q="Дюна" text="Дюна" scope="all" nothing={false} />} />
        <Route path="/play/input/:id" element={<Player />} />
      </Routes>
      <AddonWaysOutlet />
    </MemoryRouter>,
  )

const card = async () => (await screen.findAllByRole('link', { name: 'Дюна' }, { timeout: 3000 }))[0]!.closest('li')!

describe('FindAndAddBlock', () => {
  beforeEach(() => (me = { id: 2, isAdmin: false, isKid: false, lang: 'ru' }))
  afterEach(() => vi.unstubAllGlobals())

  it('watch now opens the core input player', async () => {
    const f = stub()
    renderBlock()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Смотреть' }))
    expect(await screen.findByRole('heading', { name: 'Плеер in-7: Дюна / p.jpg' })).toBeInTheDocument()
    const acq = f.mock.calls.find(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')!
    expect(JSON.parse(String(acq[1]!.body))).toEqual({ tmdbId: 1, kind: 'movie', mode: 'stream' })
    expect(f.mock.calls.some(([u, i]) => String(u).endsWith(`/x/watch/${IH}/2`) && i?.method === 'POST')).toBe(true)
  })

  it('no online way in rows', async () => {
    const f = stub()
    renderBlock()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Другие способы просмотра' }))
    // the ▾ holds the release picker only: no «Онлайн» / «Торрент» to choose from
    expect(screen.getByRole('menuitem', { name: 'Выбрать раздачу…' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitemradio')).not.toBeInTheDocument()
    expect(screen.queryByText('Онлайн')).not.toBeInTheDocument()
    expect(f.mock.calls.some(([u]) => String(u).includes('/online'))).toBe(false)
  })

  it('the admin gets «Выбрать релиз…» on an album; an adult does not', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).includes('/x/discover')
          ? json([{ tmdbId: 0, extId: 'mb-1', kind: 'music', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
          : json({}),
      ),
    )
    const { unmount } = render(
      <MemoryRouter>
        <FindAndAddBlock q="Кино" text="Кино" scope="music" nothing={false} />
      </MemoryRouter>,
    )
    expect(await screen.findByRole('button', { name: 'На сервер' }, { timeout: 3000 })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Выбрать релиз…' })).not.toBeInTheDocument()
    unmount()
    me = { id: 1, isAdmin: true, isKid: false, lang: 'ru' }
    render(
      <MemoryRouter>
        <FindAndAddBlock q="Кино" text="Кино" scope="music" nothing={false} />
      </MemoryRouter>,
    )
    await waitFor(() => expect(screen.getByRole('button', { name: 'Выбрать релиз…' })).toBeInTheDocument(), { timeout: 3000 })
  })

  it('?dk= picks the catalog the empty page asked for', async () => {
    const f = stub()
    render(
      <MemoryRouter initialEntries={['/search?dk=book&q=Пикник']}>
        <FindAndAddBlock q="Пикник" text="Пикник" scope="all" nothing={false} />
      </MemoryRouter>,
    )
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).includes('/x/discover') && String(u).includes('kind=book'))).toBe(true), { timeout: 3000 })
  })
})
