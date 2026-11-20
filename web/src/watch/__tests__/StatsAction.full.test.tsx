// «Статистика» beside «Оставить на сервере» in the input player's top bar:
// a sheet of the download as the status poll sees it.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { PlayerInput } from '@antresol/addon-sdk'
import { PlayerActions } from '../StatsAction'

const json = (b: unknown, status = 200) =>
  new Response(b === undefined ? null : JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

afterEach(() => vi.unstubAllGlobals())

const input = (ih: string): PlayerInput => ({ id: 'in-1', addonId: 'torrents', name: 'Фильм.mkv', itemId: 0, slow: true, meta: { ih, idx: 0, mode: 'stream' }, startSec: 0 })

function serveStatus(ih: string, st: object) {
  const f = vi.fn(async (u: RequestInfo | URL) =>
    String(u).includes(`/x/torrents/${ih}/status`) ? json(st) : json({ error: 'нет' }, 404),
  )
  vi.stubGlobal('fetch', f)
  return f
}

const row = (sheet: HTMLElement, label: string) => within(sheet).getByText(label).closest('li')!

describe('«Статистика» of «Смотреть сейчас»', () => {
  it('shows the release, its source, size, progress, speeds, peers and the buffer', async () => {
    const IH = '1'.repeat(40)
    serveStatus(IH, {
      status: 'downloading', mode: 'stream', size: 4 * 2 ** 30, done: 2 ** 30, downRate: 3 << 20, upRate: 64 << 10, uploaded: 10 << 20,
      peers: 12, seeds: 7, name: 'Avatar.Extended.2009.BDRip.1080p.2xRus.Eng', sourceName: 'RuTracker', bufferHave: 8 << 20, bufferWant: 16 << 20,
    })
    render(<PlayerActions input={input(IH)} />)
    expect(screen.getByRole('button', { name: 'Оставить на сервере' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Статистика' }))
    const sheet = await screen.findByRole('dialog', { name: 'Статистика раздачи' })
    expect(await within(sheet).findByText('Avatar.Extended.2009.BDRip.1080p.2xRus.Eng')).toBeInTheDocument()
    expect(row(sheet, 'Источник')).toHaveTextContent('RuTracker')
    expect(row(sheet, 'Загружено')).toHaveTextContent('25')
    expect(row(sheet, 'Сиды / участники')).toHaveTextContent('7 / 12')
    expect(row(sheet, 'Отдача')).toHaveTextContent(/всего/)
    expect(row(sheet, 'Буфер').textContent).toMatch(/\d/)
    expect(within(sheet).queryByText(/Не раздавать/)).not.toBeInTheDocument()
  })

  it('says upload is off under «Не раздавать», honestly', async () => {
    const IH = '2'.repeat(40)
    serveStatus(IH, { status: 'downloading', mode: 'stream', size: 100, done: 10, downRate: 0, upRate: 0, uploaded: 0, peers: 0, seeds: 0, noUpload: true })
    render(<PlayerActions input={input(IH)} />)
    await userEvent.click(screen.getByRole('button', { name: 'Статистика' }))
    const sheet = await screen.findByRole('dialog', { name: 'Статистика раздачи' })
    expect(await within(sheet).findByText('Выключена («Не раздавать»)')).toBeInTheDocument()
    expect(within(sheet).getByText(/сервер никому ничего не отдаёт/)).toBeInTheDocument()
    // no source known: a dash, never an address
    expect(row(sheet, 'Источник')).toHaveTextContent('—')
  })

  it('nothing for an input that is not a download', () => {
    vi.stubGlobal('fetch', vi.fn())
    const { container } = render(<PlayerActions input={{ ...input('3'.repeat(40)), meta: null }} />)
    expect(container).toBeEmptyDOMElement()
  })
})
