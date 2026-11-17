import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { setLanguagePref } from '@antresol/test-core/i18n'
import type { VoiceOverCandidate, VoiceOverDownload } from '@antresol/test-addon/types'
import { VoiceOverSheet } from '../VoiceOverSheet'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

const lostfilm: VoiceOverCandidate = {
  id: 'a1', sourceId: 1, sourceName: 'RuTracker', title: 'Миллиарды / Billions [S03] (2018) Звуковая дорожка LostFilm',
  studio: 'LostFilm', studios: ['LostFilm'], seeders: 12, size: 2.4 * 1024 ** 3,
  coverage: { have: 10, total: 10, known: true, exact: false },
}
const kubik: VoiceOverCandidate = {
  ...lostfilm, id: 'b2', title: 'Billions S03 RUS Sound (Кубик в Кубе)', studio: 'Кубик в Кубе', studios: ['Кубик в Кубе'], seeders: 3,
  coverage: { have: 0, total: 10, known: false, exact: false },
}
const video: VoiceOverCandidate = {
  ...lostfilm, id: 'v3', title: 'Миллиарды / Billions [S03] WEB-DL 1080p | LostFilm', video: true, seeders: 80, size: 40 * 1024 ** 3,
}

/** A fake server: the search, the file list of the video release, a download and its states. */
function server(opts: { candidates?: VoiceOverCandidate[]; video?: VoiceOverCandidate[]; searchError?: string } = {}) {
  const posts: { url: string; body: unknown }[] = []
  let downloads: VoiceOverDownload[] = []
  let polls = 0
  let failNext = false
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/api/addons/torrents/x/items/7/voiceovers') && (!init?.method || init.method === 'GET')) {
        if (opts.searchError) return json({ error: opts.searchError }, 502)
        return json({ candidates: opts.candidates ?? [lostfilm, kubik], videoReleases: opts.video ?? [], failed: [] })
      }
      if (url.endsWith('/api/addons/torrents/x/items/7/voiceovers/downloads')) {
        polls++
        if (failNext && downloads.length > 0) {
          failNext = false
          return json({ error: 'сеть' }, 503)
        }
        return json(downloads)
      }
      if (url.endsWith('/api/addons/torrents/x/items/7/voiceovers/files')) {
        posts.push({ url, body: JSON.parse(String(init?.body)) })
        return json({ id: 'v3', coverage: { have: 2, total: 10, known: true, exact: true }, episodes: [] })
      }
      if (url.endsWith('/api/addons/torrents/x/items/7/voiceovers') && init?.method === 'POST') {
        posts.push({ url, body: JSON.parse(String(init.body)) })
        downloads = [{
          infoHash: 'ih1', title: lostfilm.title, studio: 'LostFilm', status: 'downloading', progress: 0.4, done: false,
          episodes: [{ season: 3, episode: 1, fileId: 1 }, { season: 3, episode: 2, fileId: 2 }],
        }]
        return json({ infoHash: 'ih1', episodes: downloads[0]!.episodes })
      }
      return json({ error: 'нет' }, 404)
    }),
  )
  return {
    posts,
    polls: () => polls,
    failNext: () => {
      failNext = true
    },
    remove: () => {
      downloads = []
    },
    finish: () => {
      downloads = [{
        ...downloads[0]!, status: 'seeding', progress: 1, done: true,
        episodes: [
          { season: 3, episode: 1, fileId: 1, state: 'fitted', offsetMs: 1200, tempo: 1 },
          { season: 3, episode: 2, fileId: 2, state: 'misfit' },
          { season: 3, episode: 3, fileId: 3, state: 'unmeasured' },
          { season: 3, episode: 4, fileId: 4, state: 'had' },
        ],
      }]
    },
  }
}

describe('VoiceOverSheet', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
    act(() => setLanguagePref('ru'))
  })

  it('lists the voice-overs: studio, release, coverage, size and seeders', async () => {
    server()
    render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} />)
    const list = await screen.findByRole('list', { name: 'Найденные озвучки' })
    const rows = within(list).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    const first = within(rows[0]!)
    expect(first.getByText('LostFilm')).toBeInTheDocument()
    expect(first.getByText(lostfilm.title)).toHaveAttribute('title', lostfilm.title)
    expect(first.getByText('10 из 10 серий')).toBeInTheDocument()
    expect(first.getByText('2,4 ГБ')).toBeInTheDocument()
    expect(first.getByText('раздают: 12')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('серии не указаны')).toBeInTheDocument()
    expect(first.getByRole('button', { name: 'Скачать озвучку LostFilm' })).toBeInTheDocument()
  })

  it('downloads: the row shows the progress, then each episode’s fit', async () => {
    const s = server()
    render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} pollMs={20} />)
    const list = await screen.findByRole('list', { name: 'Найденные озвучки' })
    await userEvent.click(within(list).getByRole('button', { name: 'Скачать озвучку LostFilm' }))
    expect(s.posts).toEqual([{ url: expect.stringContaining('/voiceovers'), body: { id: 'a1' } }])
    const bar = await screen.findByRole('progressbar', { name: 'Загрузка озвучки LostFilm' })
    expect(bar).toHaveAttribute('aria-valuenow', '40')
    s.finish()
    expect(await screen.findByText('подогнано +1,2 с')).toBeInTheDocument()
    expect(screen.getByText('не подходит: другая версия')).toBeInTheDocument()
    expect(screen.getByText('не удалось проверить синхронность')).toBeInTheDocument()
    expect(screen.getByText('эта озвучка уже есть')).toBeInTheDocument()
    expect(screen.getByText('S3 E1')).toBeInTheDocument()
  })

  it('stops looking at a download removed in «Загрузки» and offers it again', async () => {
    const s = server()
    render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} pollMs={20} />)
    const list = await screen.findByRole('list', { name: 'Найденные озвучки' })
    await userEvent.click(within(list).getByRole('button', { name: 'Скачать озвучку LostFilm' }))
    await screen.findByRole('progressbar', { name: 'Загрузка озвучки LostFilm' })
    s.remove()
    expect(await within(list).findByRole('button', { name: 'Скачать озвучку LostFilm' })).toBeInTheDocument()
    const n = s.polls()
    await new Promise((r) => setTimeout(r, 120))
    expect(s.polls()).toBe(n)
  })

  it('a list that failed right after «Скачать» is asked again, not left at 0 %', async () => {
    const s = server()
    render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} pollMs={20} />)
    const list = await screen.findByRole('list', { name: 'Найденные озвучки' })
    s.failNext()
    await userEvent.click(within(list).getByRole('button', { name: 'Скачать озвучку LostFilm' }))
    await waitFor(() => expect(screen.getByRole('progressbar', { name: 'Загрузка озвучки LostFilm' })).toHaveAttribute('aria-valuenow', '40'))
  })

  it('a video release is looked into only when asked', async () => {
    const s = server({ video: [video] })
    render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} />)
    const vlist = await screen.findByRole('list', { name: 'Раздачи с видео' })
    expect(s.posts).toHaveLength(0)
    expect(within(vlist).queryByText('10 из 10 серий')).not.toBeInTheDocument()
    await userEvent.click(within(vlist).getByRole('button', { name: 'Проверить дорожки LostFilm' }))
    await waitFor(() => expect(s.posts).toEqual([{ url: expect.stringContaining('/voiceovers/files'), body: { id: 'v3' } }]))
    expect(await within(vlist).findByText('2 из 10 серий')).toBeInTheDocument()
    expect(within(vlist).getByRole('button', { name: 'Скачать озвучку LostFilm' })).toBeInTheDocument()
  })

  it('says when there is none, and the source’s message on an error', async () => {
    server({ candidates: [] })
    const { unmount } = render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} />)
    expect(await screen.findByText('Озвучек не нашлось')).toBeInTheDocument()
    unmount()
    server({ searchError: 'RuTracker: вход не удался' })
    render(<VoiceOverSheet itemId={7} title="Миллиарды" onClose={() => {}} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('RuTracker: вход не удался')
    expect(screen.getByRole('button', { name: 'Повторить' })).toBeInTheDocument()
  })

  it('in English', async () => {
    act(() => setLanguagePref('en'))
    server()
    render(<VoiceOverSheet itemId={7} title="Billions" onClose={() => {}} />)
    const list = await screen.findByRole('list', { name: 'Voice-overs found' })
    expect(within(list).getByText('10 of 10 episodes')).toBeInTheDocument()
    expect(within(list).getByText('12 seeding')).toBeInTheDocument()
  })
})
