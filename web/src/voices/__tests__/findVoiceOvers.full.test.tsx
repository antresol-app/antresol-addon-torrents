// «Найти озвучку» in the player's track menu: the addon's own
// playerTrackActions entry draws the VoiceOverSheet over the
// core's player; a kid gets no row.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { resetAddonsForTests } from '@antresol/test-registry'
import type { MediaFile } from '@antresol/test-core/api/types'
import { session } from '@antresol/test-core/state/session'

vi.mock('hls.js', () => {
  class FakeHls {
    static Events = { ERROR: 'error' }
    static ErrorTypes = { NETWORK_ERROR: 'networkError' }
    static isSupported = () => true
    on() {}
    loadSource() {}
    attachMedia() {}
    destroy() {}
  }
  return { default: FakeHls }
})

import { VideoPlayer } from '@antresol/test-core/player/VideoPlayer'

const adult = { id: 2, name: 'Мама', avatar: 'grad-1', hasPassword: false, isAdmin: false, isKid: false, maxRating: null }
const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

async function open(user: typeof adult) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/api/login')) return json(user)
      if (url.endsWith('/api/addons/torrents/x/items/40/voiceovers')) return json({ candidates: [], videoReleases: [], failed: [] })
      if (url.endsWith('/api/addons/torrents/x/items/40/voiceovers/downloads')) return json([])
      return json({})
    }),
  )
  await session.login(user.id)
  const start = vi.fn(async () => ({
    mode: 'direct', url: '/api/stream/5', startSec: 0, durationMs: 60000, audioIndex: 1,
    audio: [{ index: 1, codec: 'aac', lang: 'eng', title: '', channels: 2, default: true }], subtitles: [],
  }))
  render(
    <VideoPlayer itemId={41} title="Миллиарды" parts={[{ id: 5, name: 'e1.mkv', durationMs: 60000 } as MediaFile]} startSec={0}
      onClose={() => {}} start={start as never} trackActionItem={{ id: 40, title: 'Миллиарды', kind: 'series' }} />,
  )
  await waitFor(() => expect(start).toHaveBeenCalled())
  await userEvent.click(screen.getByRole('button', { name: 'Аудио и субтитры' }))
  return screen.findByRole('menu', { name: 'Аудио и субтитры' })
}

beforeAll(async () => {
  await import('@antresol/test-addon/index')
})
afterAll(() => resetAddonsForTests())

describe('VideoPlayer: «Найти озвучку»', () => {
  beforeEach(() => {
    session.reset()
    localStorage.clear()
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {})
  })
  afterEach(() => vi.unstubAllGlobals())

  it('the audio menu’s last row opens the sheet for the series', async () => {
    const menu = await open(adult)
    const audio = within(menu).getByRole('group', { name: 'Аудио' })
    const items = Array.from(audio.querySelectorAll<HTMLElement>('[role^=menuitem]'))
    expect(items.at(-1)).toHaveAccessibleName('Найти озвучку')
    await userEvent.click(items.at(-1)!)
    expect(await screen.findByRole('dialog', { name: 'Найти озвучку' })).toBeInTheDocument()
    expect(await screen.findByText('Озвучек не нашлось')).toBeInTheDocument()
  })

  it('a kid’s audio menu has no row of the addon', async () => {
    const menu = await open({ ...adult, id: 3, isKid: true })
    expect(within(menu).queryByRole('menuitem', { name: 'Найти озвучку' })).not.toBeInTheDocument()
    expect(screen.queryByText('Найти озвучку')).not.toBeInTheDocument()
  })
})
