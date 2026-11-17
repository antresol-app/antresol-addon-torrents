// «Оставить на сервере» in the input player's top bar (the note's actions,
// moved with the stream player).
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { PlayerInput } from '@antresol/addon-sdk'
import { KeepAction, KeepButton } from '../KeepAction'

const json = (b: unknown, status = 200) =>
  new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

type Handler = (url: string, init?: RequestInit) => Response | undefined
function stubFetch(h: Handler) {
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => h(String(input), init) ?? json({ error: 'нет' }, 404))
  vi.stubGlobal('fetch', f)
  return f
}
const posts = (f: ReturnType<typeof stubFetch>, path: string) =>
  f.mock.calls.filter(([u, i]) => String(u).endsWith(path) && i?.method === 'POST').map(([, i]) => (i?.body ? JSON.parse(String(i.body)) : {}))

afterEach(() => vi.unstubAllGlobals())

describe('«Оставить на сервере» in «Смотреть сейчас»', () => {
  it('promotes the torrent and says it waits for space on a full disk', async () => {
    const f = stubFetch((u, init) => (u.endsWith('/promote') && init?.method === 'POST' ? json({ status: 'waiting_space' }) : undefined))
    render(<KeepButton infoHash={'c'.repeat(40)} mode="stream" />)
    await userEvent.click(screen.getByRole('button', { name: 'Оставить на сервере' }))
    expect(await screen.findByRole('button', { name: 'Ждёт места на диске' })).toBeDisabled()
    expect(posts(f, '/promote')).toHaveLength(1)
  })

  it('shows «На сервере» for a torrent that is kept already', () => {
    render(<KeepButton infoHash={'c'.repeat(40)} mode="download" />)
    expect(screen.getByRole('button', { name: 'На сервере' })).toBeDisabled()
  })

  it('shows the server message when it fails', async () => {
    stubFetch(() => json({ error: 'Раздача не найдена' }, 404))
    render(<KeepButton infoHash={'c'.repeat(40)} mode="stream" />)
    await userEvent.click(screen.getByRole('button', { name: 'Оставить на сервере' }))
    expect(await screen.findByRole('button', { name: 'Не получилось' })).toHaveAttribute('title', 'Раздача не найдена')
  })
})

describe('KeepAction', () => {
  const IH = 'd'.repeat(40)
  const input = (mode: string): PlayerInput => ({ id: 'in-1', addonId: 'torrents', name: 'Фильм.mkv', itemId: 0, slow: true, meta: { ih: IH, idx: 0, mode }, startSec: 0 })

  it('keeps and shows «На сервере»', async () => {
    const f = stubFetch((u, init) => {
      if (u.endsWith(`/x/torrents/${IH}/promote`) && init?.method === 'POST') return json({ status: 'downloading' })
      if (u.includes(`/x/torrents/${IH}/status`)) return json({ status: 'downloading', mode: 'stream', size: 1, done: 0, downRate: 0, peers: 0, seeds: 0 })
      return undefined
    })
    render(<KeepAction input={input('stream')} />)
    await userEvent.click(screen.getByRole('button', { name: 'Оставить на сервере' }))
    expect(await screen.findByRole('button', { name: 'На сервере' })).toBeDisabled()
    // the adults' route, not the admin one
    expect(posts(f, `/x/torrents/${IH}/promote`)).toHaveLength(1)
    expect(f.mock.calls.some(([u]) => String(u).includes('/x/admin/'))).toBe(false)
  })

  it('a download kept already shows «На сервере» at once', () => {
    stubFetch(() => undefined)
    render(<KeepAction input={input('download')} />)
    expect(screen.getByRole('button', { name: 'На сервере' })).toBeDisabled()
  })

  it('no button for an input that is not a download', () => {
    stubFetch(() => undefined)
    const { container } = render(<KeepAction input={{ ...input('stream'), meta: null }} />)
    expect(container).toBeEmptyDOMElement()
  })
})
