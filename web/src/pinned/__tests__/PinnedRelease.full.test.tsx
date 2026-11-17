// The pinned release card: what is pinned, why a refresh kept it, the offer
// of a newer version («Заменить» / «Не сейчас»), «Обновить сейчас»,
// «Открепить» and «Добавить свою раздачу».
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Pin } from '@antresol/test-addon/types'
import { PinnedRelease } from '../PinnedRelease'

const json = (b: unknown, status = 200) => new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const pinOf = (over: Partial<Pin> = {}): Pin => ({
  sourceId: '5', link: 'https://t/far', details: '', releaseTitle: 'Far.Shore.S01.1080p', infoHash: 'a'.repeat(40), quality: '1080p',
  episodes: 8, pinnedAt: '2026-11-01T10:00:00Z', lastChecked: new Date().toISOString(), lastError: '', suggestion: null, ...over,
})

type Call = { url: string; method: string; body: unknown; headers: Headers }
function stub(answer: (url: string, method: string) => Response | undefined) {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      calls.push({ url, method, body: typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body, headers: new Headers(init?.headers) })
      return answer(url, method) ?? json({})
    }),
  )
  return calls
}

describe('PinnedRelease', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the release, its quality, episodes and the refresh’s problem', () => {
    render(<PinnedRelease kind="series" tmdbId={77} pin={pinOf({ lastError: 'В обновлённой раздаче нет части серий — оставлена прежняя' })} onChange={() => {}} />)
    const card = screen.getByRole('region', { name: 'Закреплённая раздача' })
    expect(within(card).getByText('Far.Shore.S01.1080p')).toBeInTheDocument()
    expect(within(card).getByText('1080p')).toBeInTheDocument()
    expect(within(card).getByText('8 серий')).toBeInTheDocument()
    expect(within(card).getByText(/^Проверено /)).toBeInTheDocument()
    expect(within(card).getByRole('alert')).toHaveTextContent('нет части серий')
  })

  it('the suggestion banner: «Заменить» accepts it and the card takes the new pin', async () => {
    const accepted = pinOf({ releaseTitle: 'Far.Shore.S01.2160p', quality: '2160p' })
    const calls = stub((url, method) => (method === 'POST' && url.endsWith('/api/addons/torrents/x/pinned/series/77/accept-suggestion') ? json({ pin: accepted }) : undefined))
    const onChange = vi.fn()
    render(
      <PinnedRelease
        kind="series"
        tmdbId={77}
        pin={pinOf({ suggestion: { releaseTitle: 'Far.Shore.S01.2160p', episodes: 10, found: '2026-11-13T09:00:00Z' } })}
        onChange={onChange}
      />,
    )
    const banner = screen.getByText('Нашлась новая версия раздачи — заменить?').closest('[role="status"]') as HTMLElement
    expect(within(banner).getByText('Far.Shore.S01.2160p')).toBeInTheDocument()
    await userEvent.click(within(banner).getByRole('button', { name: 'Заменить' }))
    await waitFor(() => expect(onChange).toHaveBeenCalledWith(accepted))
    expect(calls.filter((c) => c.method === 'POST').map((c) => c.url)).toEqual(['/api/addons/torrents/x/pinned/series/77/accept-suggestion'])
  })

  it('«Не сейчас» hides the offer and asks nothing', async () => {
    const calls = stub(() => undefined)
    render(<PinnedRelease kind="series" tmdbId={77} pin={pinOf({ suggestion: { releaseTitle: 'New', episodes: 10, found: '2026-11-13T09:00:00Z' } })} onChange={() => {}} />)
    await userEvent.click(screen.getByRole('button', { name: 'Не сейчас' }))
    expect(screen.queryByText('Нашлась новая версия раздачи — заменить?')).not.toBeInTheDocument()
    expect(calls).toEqual([])
  })

  it('«Обновить сейчас» and «Открепить»', async () => {
    const calls = stub((url, method) => {
      if (url.endsWith('/refresh')) return json({ pin: pinOf(), changed: false })
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return undefined
    })
    const onChange = vi.fn()
    render(<PinnedRelease kind="series" tmdbId={77} pin={pinOf()} onChange={onChange} />)
    await userEvent.click(screen.getByRole('button', { name: 'Обновить сейчас' }))
    expect(await screen.findByText('Изменений нет')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Открепить' }))
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith(null))
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual(['POST /api/addons/torrents/x/pinned/series/77/refresh', 'DELETE /api/addons/torrents/x/pinned/series/77'])
  })

  it('an own torrent is never refreshed: no «Обновить сейчас»', () => {
    render(<PinnedRelease kind="movie" tmdbId={42} pin={pinOf({ sourceId: '', link: 'magnet:?xt=urn:btih:aa', episodes: 0 })} onChange={() => {}} />)
    expect(screen.queryByRole('button', { name: 'Обновить сейчас' })).not.toBeInTheDocument()
    expect(screen.getByText('Своя раздача')).toBeInTheDocument()
  })

  it('«Добавить свою раздачу»: a magnet as JSON, a .torrent file as base64 in JSON', async () => {
    const own = pinOf({ sourceId: '', releaseTitle: 'Mine' })
    const calls = stub((url) => (url.endsWith('/own') ? json({ pin: own }) : undefined))
    const onChange = vi.fn()
    render(<PinnedRelease kind="series" tmdbId={77} pin={null} onChange={onChange} title={{ name: 'Дальний берег' }} />)
    expect(screen.getByText(/Раздача не закреплена/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Добавить свою раздачу' }))
    let sheet = await screen.findByRole('dialog', { name: 'Своя раздача' })
    expect(within(sheet).getByRole('button', { name: 'Закрепить' })).toBeDisabled()
    await userEvent.type(within(sheet).getByLabelText('Magnet или ссылка на раздачу'), 'magnet:?xt=urn:btih:bb')
    await userEvent.click(within(sheet).getByRole('button', { name: 'Закрепить' }))
    await waitFor(() => expect(onChange).toHaveBeenCalledWith(own))
    expect(calls[0]).toMatchObject({ method: 'POST', url: '/api/addons/torrents/x/pinned/series/77/own', body: { input: 'magnet:?xt=urn:btih:bb', name: 'Дальний берег' } })

    await userEvent.click(screen.getByRole('button', { name: 'Добавить свою раздачу' }))
    sheet = await screen.findByRole('dialog', { name: 'Своя раздача' })
    const file = new File(['d8:announce'], 'show.torrent', { type: 'application/x-bittorrent' })
    await userEvent.upload(within(sheet).getByTestId('own-torrent-file'), file)
    expect(within(sheet).getByText('Файл: show.torrent')).toBeInTheDocument()
    await userEvent.click(within(sheet).getByRole('button', { name: 'Закрепить' }))
    await waitFor(() => expect(calls).toHaveLength(2))
    // the proxy takes JSON only: the file's bytes as base64, with the title's name
    expect(calls[1]).toMatchObject({ method: 'POST', url: '/api/addons/torrents/x/pinned/series/77/own', body: { file: btoa('d8:announce'), name: 'Дальний берег' } })
  })

  it('a .torrent file over 8 MiB is refused before anything is sent', async () => {
    const calls = stub(() => undefined)
    render(<PinnedRelease kind="series" tmdbId={77} pin={null} onChange={() => {}} />)
    await userEvent.click(screen.getByRole('button', { name: 'Добавить свою раздачу' }))
    const sheet = await screen.findByRole('dialog', { name: 'Своя раздача' })
    const big = new File([new Uint8Array((8 << 20) + 1)], 'big.torrent', { type: 'application/x-bittorrent' })
    await userEvent.upload(within(sheet).getByTestId('own-torrent-file'), big)
    expect(within(sheet).getByRole('alert')).toHaveTextContent('Торрент-файл слишком большой')
    expect(within(sheet).getByRole('button', { name: 'Закрепить' })).toBeDisabled()
    expect(calls).toEqual([])
  })

  it('pinOwn refuses a file over 8 MiB itself, without reading or sending it', async () => {
    const calls = stub(() => undefined)
    const { pinOwn } = await import('@antresol/test-addon/api')
    const big = new File([new Uint8Array((8 << 20) + 1)], 'big.torrent')
    const read = vi.spyOn(big, 'arrayBuffer')
    await expect(pinOwn('movie', 42, { file: big })).rejects.toThrow('Торрент-файл слишком большой')
    expect(read).not.toHaveBeenCalled()
    expect(calls).toEqual([])
    // exactly 8 MiB goes
    const edge = new File([new Uint8Array(8 << 20)], 'edge.torrent')
    stub(() => json({ pin: pinOf() }))
    await expect(pinOwn('movie', 42, { file: edge })).resolves.toMatchObject({ releaseTitle: 'Far.Shore.S01.1080p' })
  })
})
