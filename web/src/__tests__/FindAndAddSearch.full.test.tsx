// «Найти и добавить» (FindAndAdd, the downloads addon's search
// block): rendered directly under a stand-in for the search page's field and
// filter, so these cases move with it and test only its behaviour.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { MemoryRouter, Route, Routes, useSearchParams } from 'react-router'
import { candidateGroups, FindAndAdd, searchedSong, type SearchScope } from '../find/FindAndAdd'
import { RequestReleaseSheet } from '../find/RequestReleaseSheet'

// who is signed in: what the stubbed /api/login answers (the page's own
// session is the core's; the block only gets `admin` from its wrapper)
let me: { isAdmin?: boolean } | null = null
const session = {
  reset: () => void (me = null),
  login: async (_id: number) => void (me = (await (await fetch('/api/login', { method: 'POST' })).json()) as { isAdmin?: boolean }),
}
const useSession = () => ({ user: me })

const user = (o: object) => ({ id: 2, name: 'Маша', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null, ...o })
const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

const EMPTY = { library: [], artists: [], albums: [], songs: [], books: [], audiobooks: [], comics: [] }

function setup(u: object, found: object = EMPTY) {
  const f = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.endsWith('/api/login')) return json(u)
    if (url.includes('/api/search')) return json({ ...EMPTY, ...found })
    if (url.includes('/x/discover') && url.includes('kind=music'))
      return json([{ tmdbId: 0, extId: 'mb-1', kind: 'music', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
    if (url.includes('/x/discover'))
      return json([{ tmdbId: 1, kind: 'movie', title: 'Дюна', year: 2021, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
    return json({})
  })
  vi.stubGlobal('fetch', f)
  return f
}

const SCOPES: [SearchScope, string][] = [
  ['all', 'Всё'],
  ['movies', 'Фильмы'],
  ['series', 'Сериалы'],
  ['music', 'Музыка'],
  ['books', 'Книги'],
  ['audiobooks', 'Аудиокниги'],
  ['comics', 'Манга'],
]

/** The page's field and filter (?q=, ?scope=) around FindAndAdd, as «Поиск» hands them to its blocks. */
function Find({ nothing = false }: { nothing?: boolean }) {
  const [params] = useSearchParams()
  const [text, setText] = useState(params.get('q') ?? '')
  const [scope, setScope] = useState<SearchScope>(SCOPES.find(([s]) => s === params.get('scope'))?.[0] ?? 'all')
  const { user } = useSession()
  const q = text.trim()
  return (
    <>
      <input type="search" value={text} onChange={(e) => setText(e.target.value)} />
      <div role="radiogroup" aria-label="Что искать">
        {SCOPES.map(([s, label]) => (
          <button key={s} type="button" role="radio" aria-checked={scope === s} onClick={() => setScope(s)}>
            {label}
          </button>
        ))}
      </div>
      {q.length >= 2 && <FindAndAdd text={text} q={q} scope={scope} nothing={nothing} admin={Boolean(user?.isAdmin)} />}
    </>
  )
}

const renderSearch = (path = '/search', nothing = false) =>
  render(
    <MemoryRouter initialEntries={[path]}>
      <Find nothing={nothing} />
    </MemoryRouter>,
  )

describe('Поиск', () => {
  beforeEach(() => session.reset())
  afterEach(() => vi.unstubAllGlobals())

  it('offers «Найти и добавить» with «Смотреть» and the download button to an adult', async () => {
    setup(user({}))
    await session.login(2)
    renderSearch()
    await userEvent.type(screen.getByRole('searchbox'), 'Дюна')
    expect(await screen.findByRole('button', { name: 'Смотреть' }, { timeout: 3000 })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Загрузить' })).toBeInTheDocument()
    // the old pair is gone; one way, but every adult has the ▾ for «Выбрать раздачу…»
    for (const name of ['Смотреть сейчас', 'На сервер', 'Выбрать релиз…'])
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Другие способы просмотра' })).toBeInTheDocument()
  })

  it('discover kind switch requests kind=music', async () => {
    const f = setup(user({}))
    await session.login(2)
    renderSearch('/search?q=Группа крови')
    await screen.findByRole('button', { name: 'Смотреть' }, { timeout: 3000 })
    const urls = () => f.mock.calls.map(([u]) => String(u)).filter((u) => u.includes('/x/discover'))
    expect(urls().every((u) => !u.includes('kind='))).toBe(true) // video: no kind
    fireEvent.click(within(screen.getByRole('radiogroup', { name: 'Что искать' })).getByRole('radio', { name: 'Музыка' }))
    await waitFor(() => expect(urls().some((u) => u.includes('kind=music'))).toBe(true))
    const row = (await screen.findByText('Кино', { selector: 'p' })).closest('li')!
    expect(within(row).getByRole('button', { name: 'На сервер' })).toBeInTheDocument()
    expect(within(row).queryByRole('button', { name: 'Смотреть' })).not.toBeInTheDocument()
    expect(within(row).queryByRole('button', { name: 'Загрузить' })).not.toBeInTheDocument()
  })

  describe('film and series cards', () => {
    const rel = {
      title: 'Dune.2021.2160p.WEB-DL', link: 'https://t/dune', size: 20 << 30, seeders: 30, leechers: 0, sourceId: 5,
      sourceName: 'RuTracker', score: 90, reasons: [], resolution: 2160,
    }
    const acquires = (f: ReturnType<typeof stubFilms>) =>
      f.mock.calls.filter(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST').map(([, i]) => JSON.parse(String(i!.body)))
    function stubFilms(u: object) {
      const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        if (url.endsWith('/api/login')) return json(u)
        if (url.includes('/api/search')) return json(EMPTY)
        if (url.includes('/x/discover'))
          return json([
            { tmdbId: 1, kind: 'movie', title: 'Дюна', year: 2021, poster: '', overview: '', inLibrary: false, libraryItemId: null },
            { tmdbId: 2, kind: 'series', title: 'Сёгун', year: 2024, poster: '', overview: '', inLibrary: false, libraryItemId: null },
          ])
        if (url.includes('/x/releases')) return json({ releases: [rel], rejected: [], failed: [] })
        if (url.endsWith('/x/acquire') && init?.method === 'POST') return json({ infoHash: 'a'.repeat(40), fileIndex: 0, streamUrl: '', status: 'downloading' })
        return json({})
      })
      vi.stubGlobal('fetch', f)
      return f
    }
    const cardOf = async (title: string) => (await screen.findAllByRole('link', { name: title }, { timeout: 3000 }))[0]!.closest('li')!
    const open = async (u: object) => {
      const f = stubFilms(u)
      await session.login(2)
      renderSearch('/search?q=Дюна')
      return f
    }

    it('a card has a ▾ with «Выбрать раздачу…» for every adult, and no other way of watching', async () => {
      await open(user({ canWatchOnline: true }))
      const card = await cardOf('Дюна')
      await userEvent.click(within(card).getByRole('button', { name: 'Другие способы просмотра' }))
      expect(screen.getByRole('menuitem', { name: 'Выбрать раздачу…' })).toBeInTheDocument() // not only the admin
      // the online way is not the search block's
      expect(screen.queryByRole('menuitemradio')).not.toBeInTheDocument()
    })

    it('?online=1 on the search page does not open the online sheet of every film card', async () => {
      const f = stubFilms(user({ canWatchOnline: true }))
      await session.login(2)
      renderSearch('/search?q=Дюна&online=1')
      await cardOf('Дюна')
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      expect(f.mock.calls.some(([u]) => String(u).includes('/api/online'))).toBe(false)
    })

    it('«Смотреть» streams the title', async () => {
      const f = await open(user({}))
      await userEvent.click(within(await cardOf('Дюна')).getByRole('button', { name: 'Смотреть' }))
      await waitFor(() => expect(acquires(f)).toEqual([{ tmdbId: 1, kind: 'movie', mode: 'stream' }]))
    })

    it('an adult who is not the admin picks a release to watch from the ▾ menu', async () => {
      const f = await open(user({}))
      const card = await cardOf('Дюна')
      await userEvent.click(within(card).getByRole('button', { name: 'Другие способы просмотра' }))
      await userEvent.click(screen.getByRole('menuitem', { name: 'Выбрать раздачу…' }))
      const sheet = await screen.findByRole('dialog')
      const row = (await within(sheet).findAllByRole('listitem'))[0]!
      await userEvent.click(within(row).getByRole('button', { name: 'Смотреть' }))
      await waitFor(() => expect(acquires(f)).toEqual([{ tmdbId: 1, kind: 'movie', mode: 'stream', releaseLink: 'https://t/dune', sourceId: 5 }]))
    })

    it('the round button opens the download sheet; the best row downloads and closes it', async () => {
      const f = await open(user({}))
      await userEvent.click(within(await cardOf('Дюна')).getByRole('button', { name: 'Загрузить' }))
      const sheet = await screen.findByRole('dialog')
      expect(await within(sheet).findByText('Лучшая раздача — выберет Antresol')).toBeInTheDocument()
      await userEvent.click(within(sheet).getAllByRole('button', { name: 'Загрузить' })[0]!)
      await waitFor(() => expect(acquires(f)).toEqual([{ tmdbId: 1, kind: 'movie', mode: 'download' }]))
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    })

    it('a series card keeps «Выбрать серию»; its download button goes to the title page', async () => {
      stubFilms(user({ isAdmin: true, canWatchOnline: true }))
      await session.login(2)
      render(
        <MemoryRouter initialEntries={['/search?q=Дюна']}>
          <Routes>
            <Route path="/search" element={<Find />} />
            <Route path="/title/series/2" element={<h1>Страница «Сёгун»</h1>} />
          </Routes>
        </MemoryRouter>,
      )
      const card = await cardOf('Сёгун')
      expect(within(card).getByRole('link', { name: 'Выбрать серию' })).toHaveAttribute('href', '/title/series/2')
      const download = within(card).getByRole('link', { name: 'Загрузить' })
      expect(download).toHaveAttribute('href', '/title/series/2')
      // a `download` link would save the page as a file instead of opening it
      expect(download).not.toHaveAttribute('download')
      for (const name of ['Смотреть', 'Другие способы просмотра', 'На сервер', 'Выбрать релиз…'])
        expect(within(card).queryByRole('button', { name })).not.toBeInTheDocument()
      await userEvent.click(download)
      expect(await screen.findByRole('heading', { name: 'Страница «Сёгун»' })).toBeInTheDocument()
    })
  })

  it('an album found by a song title shows «Песня: …» under it', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/login')) return json(user({}))
        if (url.includes('/api/search')) return json(EMPTY)
        if (url.includes('/x/discover'))
          return json([
            { tmdbId: 0, extId: 'mb-m', kind: 'music', title: 'Metallica', subtitle: 'Metallica', year: 1991, poster: '', overview: '', inLibrary: false, libraryItemId: null, matchedTrack: 'Sad But True' },
            { tmdbId: 0, extId: 'mb-s', kind: 'music', title: 'Sad but True', subtitle: 'Royal Blood', year: 2021, poster: '', overview: '', inLibrary: false, libraryItemId: null },
          ])
        return json({})
      }),
    )
    await session.login(2)
    renderSearch('/search?q=sad but true')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    const album = (await screen.findByText('Песня: Sad But True', undefined, { timeout: 3000 })).closest('li')!
    expect(within(album).getAllByText('Metallica')).toHaveLength(2) // the album, then the artist
    const single = screen.getByText('Royal Blood').closest('li')!
    expect(within(single).queryByText(/Песня:/)).not.toBeInTheDocument()
    expect(screen.getAllByText(/Песня:/)).toHaveLength(1)
  })

  it('an artist\'s query lists «Альбомы …» first, then «Другие совпадения»', async () => {
    const album = (extId: string, title: string, subtitle: string, byArtist?: string, inLibrary = false) => ({
      tmdbId: 0, extId, kind: 'music', title, subtitle, year: 1986, poster: '', overview: '', inLibrary, libraryItemId: inLibrary ? 7 : null, byArtist,
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/login')) return json(user({}))
        if (url.includes('/api/search')) return json(EMPTY)
        if (url.includes('/x/discover'))
          return json([
            album('mb-k', 'Kill ’Em All', 'Metallica', 'Metallica'),
            album('mb-p', 'Master of Puppets', 'Metallica', 'Metallica', true),
            album('mb-c', 'Metallica', 'Cloutchaser'),
          ])
        return json({})
      }),
    )
    await session.login(2)
    renderSearch('/search?q=metallica')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    const mine = (await screen.findByRole('heading', { name: 'Альбомы Metallica' }, { timeout: 3000 })).parentElement!
    expect(within(mine).getByText('Kill ’Em All')).toBeInTheDocument()
    expect(within(mine).getByText(/Уже в медиатеке/)).toBeInTheDocument()
    expect(within(mine).queryByText('Cloutchaser')).not.toBeInTheDocument()
    const rest = screen.getByRole('heading', { name: 'Другие совпадения' }).parentElement!
    expect(within(rest).getByText('Cloutchaser')).toBeInTheDocument()
  })

  it('music without an artist match has no group headings', () => {
    const c = { tmdbId: 0, extId: 'x', kind: 'music' as const, title: 'X', year: 0, poster: '', overview: '', inLibrary: false, libraryItemId: null }
    expect(candidateGroups([c])).toEqual([{ key: 'all', items: [c] }])
    expect(candidateGroups([])).toEqual([])
    expect(candidateGroups([{ ...c, byArtist: 'A' }]).map((g) => g.key)).toEqual(['artist'])
  })

  it('searchedSong: the catalog\'s song, else a query that is not the album or its artist', () => {
    const c = { title: 'Группа крови', subtitle: 'Кино' }
    expect(searchedSong({ ...c, matchedTrack: 'Война' }, 'кино война')).toBe('Война')
    expect(searchedSong(c, 'Закрой за мной дверь')).toBe('Закрой за мной дверь')
    for (const q of ['группа крови', 'Кино', 'кино — группа крови', 'Группа крови кино', '  ', '']) expect(searchedSong(c, q)).toBe('')
  })

  it('«На сервер» and the release table send the album\'s year and MusicBrainz aliases', async () => {
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ isAdmin: true }))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover'))
        return json([{ tmdbId: 0, extId: 'mb-w', kind: 'music', title: 'Weezer', subtitle: 'Weezer', year: 1994, poster: '', overview: '', inLibrary: false, libraryItemId: null, aliases: ['Blue Album'] }])
      if (url.includes('/x/releases')) return json({ releases: [], rejected: [], failed: [] })
      if (url.includes('/x/acquire') && init?.method === 'POST') return json({ status: 'downloading', infoHash: 'ab' })
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(2)
    renderSearch('/search?q=weezer')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    fireEvent.click(await screen.findByRole('button', { name: 'На сервер' }, { timeout: 3000 }))
    await waitFor(() => expect(f.mock.calls.some(([u, i]) => String(u).includes('/x/acquire') && i?.method === 'POST')).toBe(true))
    const [, init] = f.mock.calls.find(([u, i]) => String(u).includes('/x/acquire') && i?.method === 'POST')!
    expect(JSON.parse(String(init!.body))).toMatchObject({ kind: 'music', title: 'Weezer', artist: 'Weezer', year: 1994, aliases: ['Blue Album'] })

    fireEvent.click(screen.getByRole('button', { name: 'Выбрать релиз…' }))
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).includes('/x/releases'))).toBe(true))
    const url = new URL(String(f.mock.calls.find(([u]) => String(u).includes('/x/releases'))![0]), 'http://x')
    expect(url.searchParams.get('year')).toBe('1994')
    expect(url.searchParams.getAll('alias')).toEqual(['Blue Album'])
  })

  it('no release in the wanted format: «Скачать в другом формате» asks again with anyFormat', async () => {
    const bodies: Record<string, unknown>[] = []
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ isAdmin: true }))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover'))
        return json([{ tmdbId: 0, extId: 'mb-w', kind: 'music', title: 'Weezer', subtitle: 'Weezer', year: 1994, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
      if (url.includes('/x/acquire') && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as Record<string, unknown>
        bodies.push(body)
        if (!body.anyFormat) return json({ error: 'Нет варианта в нужном формате (MP3 320)', code: 'no_format' }, 404)
        return json({ status: 'downloading', infoHash: 'ab' })
      }
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(2)
    renderSearch('/search?q=weezer')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    fireEvent.click(await screen.findByRole('button', { name: 'На сервер' }, { timeout: 3000 }))
    expect(await screen.findByText(/Нет варианта в нужном формате \(MP3 320\)/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Скачать в другом формате' }))
    await waitFor(() => expect(bodies).toHaveLength(2))
    expect(bodies[1]).toMatchObject({ kind: 'music', title: 'Weezer', anyFormat: true })
    expect(bodies[0]!.anyFormat).toBeUndefined()
  })

  it('pickReleaseThenTracks', async () => {
    const release = (o: object) => ({ sourceId: 7, sourceName: 'Трекер', title: 'Кино - Группа крови (1988) [FLAC]', size: 300 << 20, seeders: 10, leechers: 0, link: 'https://t/2.torrent', score: 1, ...o })
    const files = {
      infoHash: 'ab'.repeat(20),
      name: 'Кино - Группа крови (1988) [FLAC]',
      added: false,
      files: [
        { index: 0, path: 'a/01.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true },
        { index: 1, path: 'a/02.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true },
        { index: 2, path: 'a/cover.jpg', size: 1000, kind: 'extra', album: 'Группа крови (1988)', checked: true },
      ],
    }
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ isAdmin: true }))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover'))
        return json([{ tmdbId: 0, extId: 'mb-1', kind: 'music', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
      if (url.includes('/x/releases'))
        return json({ releases: [release({}), release({ title: 'Кино - Группа крови (файл)', link: 'https://opds/1.zip', sourceId: 8, direct: true })], rejected: [], failed: [] })
      if (url.endsWith('/x/admin/acquire/files')) return json(files)
      if (url.endsWith('/x/acquire') && init?.method === 'POST') return json({ status: 'downloading', infoHash: 'ab'.repeat(20), fileIndex: -1, streamUrl: '' })
      return json({})
    })
    vi.stubGlobal('fetch', f)
    const acquires = () => f.mock.calls.filter(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST').map(([, i]) => JSON.parse(String(i!.body)))
    await session.login(2)
    renderSearch('/search?q=кино')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Выбрать релиз…' }, { timeout: 3000 }))
    let sheet = await screen.findByRole('dialog')
    const list = await within(sheet).findByRole('list', { name: 'Раздачи' })
    // a direct file has no file list: no «Выбрать треки…» on its row
    expect(within(within(list).getAllByRole('listitem')[1]!).queryByRole('button', { name: /^Выбрать треки/ })).not.toBeInTheDocument()
    const releaseButton = within(within(list).getAllByRole('listitem')[0]!).getByRole('button', { name: 'Выбрать треки — Кино - Группа крови (1988) [FLAC]' })
    releaseButton.focus()
    fireEvent.click(releaseButton)
    // the track step, nothing acquired yet; focus moved into it, not to the page
    expect(await within(sheet).findByText('Песен: 0 из 2')).toBeInTheDocument()
    await waitFor(() => expect(document.activeElement).toBe(within(sheet).getByRole('checkbox', { name: 'Группа крови (1988)' })))
    expect(acquires()).toHaveLength(0)
    const filesCall = f.mock.calls.find(([u]) => String(u).endsWith('/x/admin/acquire/files'))!
    const sent = JSON.parse(String(filesCall[1]!.body)) as Record<string, unknown>
    expect(sent).toMatchObject({ sourceId: 7, releaseLink: 'https://t/2.torrent', title: 'Группа крови', artist: 'Кино', year: 1988 })
    // the query «кино» is the artist, no song
    expect(sent).not.toHaveProperty('song')
    // «Назад» shows the release table again
    fireEvent.click(within(sheet).getByRole('button', { name: 'Назад' }))
    expect(await within(sheet).findByRole('list', { name: 'Раздачи' })).toBeInTheDocument()
    expect(within(sheet).queryByText('Песен: 0 из 2')).not.toBeInTheDocument()
    expect(document.activeElement).toBe(releaseButton)
    fireEvent.click(within(within(within(sheet).getByRole('list', { name: 'Раздачи' })).getAllByRole('listitem')[0]!).getByRole('button', { name: /^Выбрать треки/ }))
    fireEvent.click(await within(sheet).findByRole('checkbox', { name: '01' }))
    fireEvent.click(within(sheet).getByRole('button', { name: 'Скачать выбранные (1)' }))
    await waitFor(() => expect(acquires()).toHaveLength(1))
    expect(acquires()[0]).toMatchObject({ kind: 'music', mode: 'download', files: [0, 2], releaseLink: 'https://t/2.torrent', sourceId: 7, releaseTitle: 'Кино - Группа крови (1988) [FLAC]' })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // the release's «На сервер»: the file list says it is this album alone, so it goes whole (no files)
    fireEvent.click(screen.getByRole('button', { name: 'Выбрать релиз…' }))
    sheet = await screen.findByRole('dialog')
    fireEvent.click(within(within(await within(sheet).findByRole('list', { name: 'Раздачи' })).getAllByRole('listitem')[0]!).getByRole('button', { name: 'На сервер' }))
    await waitFor(() => expect(acquires()).toHaveLength(2))
    expect(acquires()[1]).toMatchObject({ kind: 'music', mode: 'download', releaseLink: 'https://t/2.torrent', sourceId: 7, releaseTitle: 'Кино - Группа крови (1988) [FLAC]' })
    expect(acquires()[1]).not.toHaveProperty('files')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // the row's one-tap «На сервер»: no files
    fireEvent.click(screen.getByRole('button', { name: 'На сервер' }))
    await waitFor(() => expect(acquires()).toHaveLength(3))
    expect(acquires()[2]).not.toHaveProperty('files')
    expect(acquires()[2]).not.toHaveProperty('releaseLink')

    // a direct release acquires at once
    fireEvent.click(screen.getByRole('button', { name: 'Выбрать релиз…' }))
    sheet = await screen.findByRole('dialog')
    const items = within(await within(sheet).findByRole('list', { name: 'Раздачи' })).getAllByRole('listitem')
    fireEvent.click(within(items[1]!).getByRole('button', { name: 'На сервер' }))
    await waitFor(() => expect(acquires()).toHaveLength(4))
    expect(acquires()[3]).toMatchObject({ releaseLink: 'https://opds/1.zip', sourceId: 8, direct: true })
    expect(acquires()[3]).not.toHaveProperty('files')
    expect(f.mock.calls.filter(([u]) => String(u).endsWith('/x/admin/acquire/files'))).toHaveLength(3)
  })

  it('a searched song is sent with the file list; an owned album offers only «Выбрать треки…»', async () => {
    const release = { sourceId: 7, sourceName: 'Трекер', title: 'Кино - Группа крови (1988) [FLAC]', size: 300 << 20, seeders: 10, leechers: 0, link: 'https://t/2.torrent', score: 1 }
    const files = {
      infoHash: 'ab'.repeat(20),
      name: 'Кино - Группа крови (1988) [FLAC]',
      added: false,
      files: [
        { index: 0, path: 'a/01 Группа крови.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true, no: 1 },
        { index: 1, path: 'a/02 Война.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true, no: 2, matched: true },
      ],
    }
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ isAdmin: true }))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover'))
        return json([
          { tmdbId: 0, extId: 'mb-1', kind: 'music', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', overview: '', inLibrary: true, libraryItemId: 10, matchedTrack: 'Война' },
        ])
      if (url.includes('/x/releases')) return json({ releases: [release], rejected: [], failed: [] })
      if (url.endsWith('/x/admin/acquire/files')) return json(files)
      if (url.endsWith('/x/acquire') && init?.method === 'POST') return json({ status: 'downloading', infoHash: 'ab'.repeat(20), fileIndex: -1, streamUrl: '' })
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(2)
    renderSearch('/search?q=война')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    // the album is on the server: «Уже в медиатеке» and, for the admin, «Выбрать треки…» to add songs to it
    expect(await screen.findByRole('link', { name: 'Уже в медиатеке ›' }, { timeout: 3000 })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Выбрать релиз…' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Выбрать треки…' }))
    const sheet = await screen.findByRole('dialog')
    const row = within(await within(sheet).findByRole('list', { name: 'Раздачи' })).getAllByRole('listitem')[0]!
    // no second whole copy of an album the library has
    expect(within(row).queryByRole('button', { name: 'На сервер' })).not.toBeInTheDocument()
    fireEvent.click(within(row).getByRole('button', { name: /^Выбрать треки/ }))
    const song = await within(sheet).findByRole('checkbox', { name: 'Война' })
    expect(song).toBeChecked()
    expect(within(sheet).getByRole('checkbox', { name: 'Группа крови' })).not.toBeChecked()
    const filesCall = f.mock.calls.find(([u]) => String(u).endsWith('/x/admin/acquire/files'))!
    expect(JSON.parse(String(filesCall[1]!.body))).toMatchObject({ song: 'Война' })
    fireEvent.click(within(sheet).getByRole('button', { name: 'Скачать выбранные (1)' }))
    await waitFor(() => expect(f.mock.calls.some(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')).toBe(true))
    const [, init] = f.mock.calls.find(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')!
    expect(JSON.parse(String(init!.body))).toMatchObject({ files: [1], releaseLink: 'https://t/2.torrent' })
  })

  it('«На сервер» on a discography opens the track step with the requested album checked', async () => {
    const release = { sourceId: 7, sourceName: 'Трекер', title: 'Кино - Дискография [FLAC]', size: 3000 << 20, seeders: 10, leechers: 0, link: 'https://t/d.torrent', score: 1 }
    const files = {
      infoHash: 'cd'.repeat(20),
      name: 'Кино - Дискография [FLAC]',
      added: false,
      files: [
        { index: 0, path: 'd/1988/01 Группа крови.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true },
        { index: 1, path: 'd/1989/01 Звезда.flac', size: 1 << 20, kind: 'track', album: 'Звезда по имени Солнце (1989)', checked: false },
      ],
    }
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ isAdmin: true }))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover'))
        return json([{ tmdbId: 0, extId: 'mb-1', kind: 'music', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
      if (url.includes('/x/releases')) return json({ releases: [release], rejected: [], failed: [] })
      if (url.endsWith('/x/admin/acquire/files')) return json(files)
      if (url.endsWith('/x/acquire') && init?.method === 'POST') return json({ status: 'downloading', infoHash: 'cd'.repeat(20), fileIndex: -1, streamUrl: '' })
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(2)
    renderSearch('/search?q=кино')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Выбрать релиз…' }, { timeout: 3000 }))
    const sheet = await screen.findByRole('dialog')
    fireEvent.click(within(within(await within(sheet).findByRole('list', { name: 'Раздачи' })).getAllByRole('listitem')[0]!).getByRole('button', { name: 'На сервер' }))
    expect(await within(sheet).findByRole('checkbox', { name: 'Группа крови' })).toBeChecked()
    expect(within(sheet).getByRole('checkbox', { name: 'Звезда' })).not.toBeChecked()
    fireEvent.click(within(sheet).getByRole('button', { name: 'Скачать выбранные (1)' }))
    await waitFor(() => expect(f.mock.calls.some(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')).toBe(true))
    const [, init] = f.mock.calls.find(([u, i]) => String(u).endsWith('/x/acquire') && i?.method === 'POST')!
    expect(JSON.parse(String(init!.body))).toMatchObject({ files: [0], releaseLink: 'https://t/d.torrent' })
  })

  it('trackStepConfirmFails', async () => {
    const release = { sourceId: 7, sourceName: 'Трекер', title: 'Кино - Группа крови (1988) [FLAC]', size: 300 << 20, seeders: 10, leechers: 0, link: 'https://t/2.torrent', score: 1 }
    const files = {
      infoHash: 'ab'.repeat(20),
      name: 'Кино - Группа крови (1988) [FLAC]',
      added: false,
      files: [
        { index: 0, path: 'a/01.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true },
        { index: 1, path: 'a/02.flac', size: 1 << 20, kind: 'track', album: 'Группа крови (1988)', checked: true },
      ],
    }
    let answer: (r: Response) => void = () => {}
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ isAdmin: true }))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover'))
        return json([{ tmdbId: 0, extId: 'mb-1', kind: 'music', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', overview: '', inLibrary: false, libraryItemId: null }])
      if (url.includes('/x/releases')) return json({ releases: [release], rejected: [], failed: [] })
      if (url.endsWith('/x/admin/acquire/files')) return json(files)
      if (url.endsWith('/x/acquire') && init?.method === 'POST') return new Promise<Response>((res) => (answer = res))
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(2)
    renderSearch('/search?q=кино')
    fireEvent.click(within(await screen.findByRole('radiogroup', { name: 'Что искать' }, { timeout: 3000 })).getByRole('radio', { name: 'Музыка' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Выбрать релиз…' }, { timeout: 3000 }))
    const sheet = await screen.findByRole('dialog')
    const list = await within(sheet).findByRole('list', { name: 'Раздачи' })
    fireEvent.click(within(within(list).getAllByRole('listitem')[0]!).getByRole('button', { name: /^Выбрать треки/ }))
    fireEvent.click(await within(sheet).findByRole('checkbox', { name: 'Группа крови (1988)' }))
    await within(sheet).findByText('Песен: 2 из 2')
    const confirm = within(sheet).getByRole('button', { name: 'Скачать выбранные (2)' })
    confirm.focus()
    fireEvent.click(confirm)
    // while the acquire runs «На сервер» is disabled: focus stays inside the sheet
    await waitFor(() => expect(confirm).toBeDisabled())
    expect(sheet.contains(document.activeElement)).toBe(true)
    answer(new Response(JSON.stringify({ error: 'Эта раздача уже добавлена' }), { status: 409, headers: { 'Content-Type': 'application/json' } }))
    // the failure shows next to «На сервер» (the footer), once, and focus is back on the button
    const alert = await within(sheet).findByRole('alert')
    expect(alert).toHaveTextContent('Эта раздача уже добавлена')
    expect(within(sheet).getAllByRole('alert')).toHaveLength(1)
    const footer = confirm.parentElement!.parentElement!
    expect(footer.contains(alert)).toBe(true)
    await waitFor(() => expect(document.activeElement).toBe(confirm))
    expect(confirm).toBeEnabled()
  })

  it('nothing in the library nor the catalog: no empty «Найти и добавить»', async () => {
    const f = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({}))
      if (url.includes('/api/search')) return json(EMPTY)
      if (url.includes('/x/discover')) return json([])
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(2)
    // the library found nothing either: the page's one empty state says it
    renderSearch('/search?q=zzzz', true)
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).includes('/x/discover'))).toBe(true), { timeout: 3000 })
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Найти и добавить' })).not.toBeInTheDocument())
    expect(screen.queryByText('Ничего не нашлось.')).not.toBeInTheDocument()
  })

  it('«Сериалы» narrows «Найти и добавить» to series, and it says «Сериалы»', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.endsWith('/api/login')) return json({ id: 2, name: 'М', avatar: 'grad-3', hasPassword: false, isAdmin: false, isKid: false, maxRating: null })
        if (url.includes('/x/discover'))
          return json([
            { tmdbId: 7, kind: 'movie', title: 'Dune: Part Three', year: 2026, poster: '', overview: '', inLibrary: false, libraryItemId: null },
            { tmdbId: 8, kind: 'series', title: 'Dune: Prophecy II', year: 2026, poster: '', overview: '', inLibrary: false, libraryItemId: null },
          ])
        return json({})
      }),
    )
    await session.login(2)
    renderSearch('/search?q=дюна&scope=series')
    const add = await screen.findByRole('region', { name: 'Найти и добавить' }, { timeout: 3000 })
    expect(await within(add).findByText('Dune: Prophecy II', {}, { timeout: 3000 })).toBeInTheDocument()
    expect(within(add).queryByText('Dune: Part Three')).not.toBeInTheDocument()
    expect(within(add).getByText('Сериалы')).toBeInTheDocument()
  })

  it('«Одобрить»\'s release picker asks for the requested album\'s releases', async () => {
    const f = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user({ id: 1, isAdmin: true }))
      if (url.includes('/x/releases')) return json({ releases: [], rejected: [], failed: [] })
      return json({})
    })
    vi.stubGlobal('fetch', f)
    await session.login(1)
    const request = {
      id: 2, key: 'ext:mb-1', kind: 'music', tmdbId: 0, extId: 'mb-1', title: 'Группа крови', subtitle: 'Кино', year: 1988, poster: '', aliases: [],
      rating: '', status: 'pending' as const, reason: '', libraryItemId: null, createdAt: new Date().toISOString(),
    }
    render(
      <MemoryRouter>
        <RequestReleaseSheet request={request} onClose={() => {}} onApproved={() => {}} />
      </MemoryRouter>,
    )
    expect(await screen.findByRole('dialog', { name: /Кино — Группа крови \(1988\)/ })).toBeInTheDocument()
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).includes('/x/releases') && String(u).includes('extId=mb-1'))).toBe(true))
  })
})
