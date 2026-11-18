// The addon's player of the episode guide (episodeActions, kinds pinned and
// torrent): both streams become an input of the core's player
// (/play/input/<id>).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router'
import type { EpisodePlay, EpisodeSource, TitleEpisode } from '@antresol/addon-sdk'
import { ep } from '@antresol/test-core/components/episodes/__tests__/fixtures'
import { session } from '@antresol/test-core/state/session'
import { writeWatchWay } from '@antresol/test-core/state/watchWay'
import { useEpisodePlay, WATCH_WAY } from '../useEpisodePlay'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const adult = { id: 2, name: 'Test', avatar: 'grad-1', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
const IH = 'a'.repeat(40)

const pinned = (fileIndex = 3): EpisodeSource => ({ kind: 'pinned', ref: `pin:1:${fileIndex}`, quality: '1080p', labels: [], firstSeen: '2026-01-01', meta: { fileIndex } })
const torrent = (): EpisodeSource => ({ kind: 'torrent', ref: 'https://t/9', provider: 'Tracker', quality: '1080p', labels: [], firstSeen: '2026-01-01', meta: { sourceId: '3' } })

function serve(user: Record<string, unknown> = adult) {
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/x/pinned/') && url.endsWith('/play')) return json({ inputId: 'pin-in' })
    if (url.includes(`/x/watch/${IH}/`)) return json({ inputId: 'tor-in' })
    if (url.includes('/x/pinned/') && url.endsWith('/download')) return json({ infoHash: IH, fileIndex: -1, streamUrl: '', status: 'downloading', note: 'Серия 2 добавлена к загрузке' })
    if (url.endsWith('/x/acquire') && init?.method === 'POST') return json({ infoHash: IH, fileIndex: 0, streamUrl: '', status: 'downloading' })
    if (url.includes('/x/releases')) return json({ releases: [], rejected: [], failed: [] })
    return json(user)
  })
  vi.stubGlobal('fetch', f)
  return f
}
const posts = (f: ReturnType<typeof serve>, end: string) =>
  f.mock.calls.filter(([u, i]) => String(u).split('?')[0]!.endsWith(end) && i?.method === 'POST').map(([, i]) => JSON.parse(String(i!.body)))

const Where = () => {
  const l = useLocation()
  return <output data-testid="where" data-state={JSON.stringify(l.state ?? null)}>{l.pathname}</output>
}
let hook: EpisodePlay | null = null
function Harness({ episodes }: { episodes: TitleEpisode[] }) {
  const p = useEpisodePlay({ kind: 'series', tmdbId: 5, title: 'Show', episodes })
  hook = p
  return <>{p?.node}</>
}
const mount = (episodes: TitleEpisode[]) =>
  render(
    <MemoryRouter>
      <Harness episodes={episodes} />
      <Where />
    </MemoryRouter>,
  )
const where = () => screen.getByTestId('where')

describe('useEpisodePlay', () => {
  beforeEach(async () => {
    session.reset()
    hook = null
    serve()
    await session.login(2)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('pinned source plays the pin', async () => {
    const f = serve()
    const e = ep(2, { sources: [pinned()] })
    mount([e])
    act(() => hook!.play(e, e.sources[0]!))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/pin-in'))
    expect(JSON.parse(where().dataset.state!)).toEqual({ title: 'Show' })
    expect(posts(f, '/x/pinned/series/5/play')).toEqual([{ season: 1, episode: 2 }])
    // the pin's play answers the input itself: nothing through x/acquire
    expect(posts(f, '/x/acquire')).toEqual([])
  })


  it('torrent source acquires that release', async () => {
    const f = serve()
    const e = ep(2, { sources: [torrent()] })
    mount([e])
    act(() => hook!.play(e, e.sources[0]!))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/tor-in'))
    expect(posts(f, '/x/acquire')).toEqual([{ tmdbId: 5, kind: 'series', mode: 'stream', season: 1, episode: 2, releaseLink: 'https://t/9', sourceId: 3 }])
  })


  it('other sources opens the release table for the episode', async () => {
    const f = serve()
    const e = ep(2, { sources: [pinned()] })
    mount([e])
    act(() => hook!.other(e))
    const sheet = await screen.findByRole('dialog')
    expect(within(sheet).getByText(/серию 2/)).toBeInTheDocument()
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).includes('/x/releases') && String(u).includes('season=1') && String(u).includes('episode=2'))).toBe(true))
  })


  it('pinned download posts one episode', async () => {
    const f = serve()
    const e = ep(2, { sources: [pinned()] })
    mount([e])
    act(() => hook!.download!(e, e.sources[0]!))
    expect(await screen.findByText('Серия 2 добавлена к загрузке')).toBeInTheDocument()
    expect(posts(f, '/x/pinned/series/5/download')).toEqual([{ season: 1, episodes: [2] }])
  })

  it('prefers the pin and the release once «Смотреть сейчас» is the remembered way', async () => {
    const e = ep(2, { sources: [pinned()] })
    mount([e])
    expect(hook!.prefer).toBeUndefined()
    localStorage.setItem('antresol.watchWay.2', WATCH_WAY)
    const again = mount([e])
    expect(hook!.prefer).toEqual(['pinned', 'torrent'])
    again.unmount()
    localStorage.clear()
  })

  it('follows the remembered way live, through the SDK (no remount)', async () => {
    localStorage.clear()
    const e = ep(2, { sources: [pinned()] })
    mount([e])
    expect(hook!.prefer).toBeUndefined()
    act(() => writeWatchWay(2, WATCH_WAY))
    await waitFor(() => expect(hook!.prefer).toEqual(['pinned', 'torrent']))
    act(() => writeWatchWay(2, 'online'))
    await waitFor(() => expect(hook!.prefer).toBeUndefined())
    localStorage.clear()
  })

  it('a kid gets no player', async () => {
    session.reset()
    serve({ ...adult, id: 3, isKid: true })
    await session.login(3)
    mount([ep(2, { sources: [pinned()] })])
    expect(hook).toBeNull()
  })
})
