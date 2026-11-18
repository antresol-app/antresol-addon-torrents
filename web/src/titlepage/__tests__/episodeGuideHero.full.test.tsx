// A TMDB series whose episode guide knows the next episode: the core's
// «Смотреть С1 Э2» plays it, a torrent or pinned source through the addon's
// episodeActions, over another addon's source kind when the profile chose
// «Смотреть сейчас» last; «Другие источники…» opens the addon's release table
// when the guide lists its kinds.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { registerAddon, resetAddonsForTests } from '@antresol/test-registry'
import type { EpisodeSource, SourceChoice, TitleEpisodes } from '@antresol/test-core/api/types'
import { ep, online } from '@antresol/test-core/components/episodes/__tests__/fixtures'
import { session } from '@antresol/test-core/state/session'
import { WATCH_WAY } from '@antresol/test-addon/episodes/useEpisodePlay'
import { adult, json, posts, registerTheAddon, renderAt, serve, signIn, X, type Call } from './page'

const IH = 'c'.repeat(40)
const show = {
  tmdbId: 77, kind: 'series', title: 'Сёгун', year: 2024, poster: '', backdrop: '', overview: '', inLibrary: false,
  libraryItemId: null, genres: [], seasons: [{ number: 1, episodes: 3 }],
}
const torrent: EpisodeSource = { kind: 'torrent', ref: 'https://t/ep2', provider: 'Tracker', quality: '1080p', labels: [], firstSeen: '2026-01-01', meta: { sourceId: '4' } }
const pinned: EpisodeSource = { kind: 'pinned', ref: 'pin:1:2', quality: '1080p', labels: [], firstSeen: '2026-01-01', meta: { fileIndex: 2 } }
const guideOf = (episodes: TitleEpisodes['episodes']): TitleEpisodes => ({
  title: { kind: 'series', tmdbId: 77, name: 'Сёгун', poster: '', status: 'returning' }, episodes,
})

async function open(guide: TitleEpisodes, choice: SourceChoice | null = null, user: { id: number } = adult) {
  const calls = serve(
    user,
    (url, method) => {
      if (url.endsWith('/episodes')) return json(guide)
      if (url.endsWith('/choice')) return json({ choice })
      if (url.endsWith(`${X}acquire`) && method === 'POST') return json({ infoHash: IH, fileIndex: 0, streamUrl: '', status: 'downloading' })
      if (url.includes(`${X}watch/${IH}/`)) return json({ inputId: 'tor-in' })
      if (url.endsWith(`${X}pinned/series/77/play`)) return json({ inputId: 'pin-in' })
      return undefined
    },
    { title: show },
  )
  await signIn(user)
  renderAt('/title/series/77')
  return calls
}
const acquires = (calls: Call[]) => posts(calls, `${X}acquire`)
const where = () => screen.getByTestId('where')

beforeAll(registerTheAddon)
afterAll(() => resetAddonsForTests())
beforeEach(() => localStorage.clear())
afterEach(() => {
  vi.unstubAllGlobals()
  session.reset()
})

describe('the title page’s next episode', () => {
  it('plays a torrent source chosen for the title through the addon', async () => {
    const calls = await open(
      guideOf([
        ep(1, { watched: true, sources: [online('alpha', 'Voice A')] }),
        ep(2, { sources: [online('alpha', 'Voice A'), torrent] }),
        ep(3, { airDate: '2099-01-01' }),
      ]),
      { kind: 'torrent', provider: 'Tracker', quality: '1080p' },
    )
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть С1 Э2' }))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/tor-in'))
    expect(acquires(calls)).toEqual([{ tmdbId: 77, kind: 'series', mode: 'stream', season: 1, episode: 2, releaseLink: 'https://t/ep2', sourceId: 4 }])
  })

  it('plays a pinned source from the pin', async () => {
    const calls = await open(guideOf([ep(2, { sources: [pinned] })]))
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть С1 Э2' }))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/pin-in'))
    expect(posts(calls, `${X}pinned/series/77/play`)).toEqual([{ season: 1, episode: 2 }])
  })

  it('no choice, «Смотреть сейчас» remembered: the torrent over a source kind another addon plays', async () => {
    // the online kind has a player of another addon, ranked ahead of the torrent (20 < 30): the way's preference wins
    const played: string[] = []
    registerAddon('online', {
      episodeActions: [{ kinds: ['online'], order: { online: 20 }, use: () => ({ play: () => void played.push('online'), other: () => {}, node: null }) }],
    })
    localStorage.setItem('antresol.watchWay.2', WATCH_WAY)
    const calls = await open(guideOf([ep(2, { sources: [online('alpha', 'Voice A'), torrent] })]))
    await userEvent.click(await screen.findByRole('button', { name: 'Смотреть С1 Э2' }))
    await waitFor(() => expect(where()).toHaveTextContent('/play/input/tor-in'))
    expect(acquires(calls)).toHaveLength(1)
    expect(played).toEqual([])
  })

  it('▾ «Другие источники…» opens the addon’s release table when the guide lists its kind', async () => {
    const calls = await open(guideOf([ep(2, { sources: [torrent] })]), null, adult)
    await screen.findByRole('button', { name: 'Смотреть С1 Э2' })
    await userEvent.click(screen.getByRole('button', { name: 'Другие способы просмотра' }))
    await userEvent.click(screen.getByRole('menuitem', { name: 'Другие источники…' }))
    const sheet = await screen.findByRole('dialog')
    expect(within(sheet).getByText(/серию 2/)).toBeInTheDocument()
    await waitFor(() => expect(calls.some((c) => c.url.includes(`${X}releases`) && c.url.includes('episode=2'))).toBe(true))
  })
})
