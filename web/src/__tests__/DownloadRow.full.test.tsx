// A row of «Загрузки»: its state decides the status colour, the bar, the one
// action in the trailing slot and what «⋯» offers.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { DownloadRow } from '../pages/DownloadRow'
import { setViewport } from './viewport'

const GB = 1024 ** 3
const tor = (o: Partial<Torrent>): Torrent => ({
  infoHash: 'a'.repeat(40),
  name: 'Dune.2021.2160p',
  title: 'Дюна',
  mode: 'download',
  status: 'downloading',
  size: 30 * GB,
  done: 3 * GB,
  downRate: 0,
  upRate: 0,
  uploaded: 0,
  downloaded: 0,
  peers: 0,
  seeds: 0,
  ratio: 0,
  itemId: null,
  addedAt: '2026-11-01T10:00:00Z',
  saveDir: '/t',
  organized: false,
  kind: 'movie',
  ...o,
})

const show = (t: Torrent) =>
  render(
    <MemoryRouter>
      <ul>
        <DownloadRow t={t} onChanged={() => {}} />
      </ul>
    </MemoryRouter>,
  )

afterEach(() => vi.unstubAllGlobals())

describe('DownloadRow', () => {
  it('a downloading row: blue state, a bar, Pause in the slot', () => {
    show(tor({}))
    const row = screen.getByTestId('torrent-card')
    expect(row).toHaveAttribute('data-state', 'downloading')
    expect(within(row).getByRole('progressbar')).toBeInTheDocument()
    expect(within(row).getByRole('button', { name: 'Пауза' })).toBeInTheDocument()
  })

  it('waiting for space: «Загрузить всё равно» with the download glyph, not the Play of a paused one', () => {
    show(tor({ status: 'waiting_space' }))
    const b = screen.getByRole('button', { name: 'Загрузить всё равно' })
    expect(b.querySelector('svg')?.getAttribute('data-icon')).toBe('arrowDown')
  })

  it('«⋯» repeats the row action before «Удалить…», never a menu of one item', async () => {
    show(tor({}))
    await userEvent.click(screen.getByRole('button', { name: 'Действия: Дюна' }))
    expect(screen.getAllByRole('menuitem').map((m) => m.textContent)).toEqual(['Пауза', 'Удалить…'])
  })

  it('a paused stream can still go to the server from «⋯»', async () => {
    show(tor({ mode: 'stream', status: 'paused' }))
    await userEvent.click(screen.getByRole('button', { name: 'Действия: Дюна' }))
    expect(screen.getByRole('menuitem', { name: 'На сервер' })).toBeInTheDocument()
  })

  it('seeding is quiet: no bar, no Pause; «Приостановить раздачу» is in «⋯»', async () => {
    show(tor({ status: 'seeding', done: 30 * GB }))
    const row = screen.getByTestId('torrent-card')
    expect(within(row).queryByRole('progressbar')).toBeNull()
    expect(within(row).queryByRole('button', { name: 'Пауза' })).toBeNull()
    await userEvent.click(within(row).getByRole('button', { name: 'Действия: Дюна' }))
    expect(screen.getByRole('menuitem', { name: 'Приостановить раздачу' })).toBeInTheDocument()
  })

  it('a write error (a full disk) is retried: «Повторить» resumes it, which clears that error', async () => {
    const f = vi.fn(async (_u: RequestInfo | URL, _i?: RequestInit) => new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', f)
    show(tor({ status: 'error', error: 'ошибка записи: no space left on device' }))
    const row = screen.getByTestId('torrent-card')
    expect(row).toHaveAttribute('data-state', 'error')
    expect(within(row).queryByRole('progressbar')).toBeNull()
    expect(within(row).getByText('ошибка записи: no space left on device')).toHaveAttribute('data-tone', 'error')
    await userEvent.click(within(row).getByRole('button', { name: 'Повторить' }))
    await waitFor(() => expect(f.mock.calls.some(([u]) => String(u).endsWith('/resume'))).toBe(true))
  })

  it('an error resuming cannot clear offers no «Повторить»: another release where the title has one', async () => {
    show(tor({ status: 'error', error: "this torrent's files are the same as torrent 366a1e9e", tmdbId: 438631 }))
    const row = screen.getByTestId('torrent-card')
    expect(within(row).queryByRole('button', { name: 'Повторить' })).toBeNull()
    expect(within(row).getByRole('button', { name: 'Выбрать другую раздачу…' })).toBeInTheDocument()
    await userEvent.click(within(row).getByRole('button', { name: 'Действия: Дюна' }))
    expect(screen.getAllByRole('menuitem').map((m) => m.textContent)).toEqual(['Выбрать другую раздачу…', 'Удалить…'])
  })

  it('…else «Удалить» is the row\'s action, not a menu of one item', () => {
    show(tor({ status: 'error', error: 'Трекер не отвечает', kind: 'music' }))
    const row = screen.getByTestId('torrent-card')
    expect(within(row).queryByRole('button', { name: 'Повторить' })).toBeNull()
    expect(within(row).queryByRole('button', { name: 'Действия: Дюна' })).toBeNull()
    expect(within(row).getByRole('button', { name: 'Удалить «Дюна»…' })).toBeInTheDocument()
  })

  it('phone: one trailing «⋯» (the row’s action is in it and a swipe away) and a short status line', async () => {
    setViewport(390)
    try {
      show(tor({ addedByName: 'Саша', peers: 15 }))
      const row = screen.getByTestId('torrent-card')
      expect(within(row).queryByRole('button', { name: 'Пауза' })).toBeNull()
      expect(row).not.toHaveTextContent('Добавил')
      // the bar shows how far; the peers are detail
      expect(row).not.toHaveTextContent('%')
      expect(row).not.toHaveTextContent('участник')
      await userEvent.click(within(row).getByRole('button', { name: /^Действия/ }))
      expect(screen.getByRole('menuitem', { name: 'Пауза' })).toBeInTheDocument()
    } finally {
      setViewport(1440)
    }
  })

  it('wide screen: the row’s action sits in the slot and the status says who added it', () => {
    show(tor({ addedByName: 'Саша' }))
    const row = screen.getByTestId('torrent-card')
    expect(within(row).getByRole('button', { name: 'Пауза' })).toBeInTheDocument()
    expect(row).toHaveTextContent('Добавил: Саша')
  })

  it('phone: while the row’s action runs, «⋯» shows it is busy (the slot’s spinner is not there)', async () => {
    setViewport(390)
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})))
    try {
      show(tor({}))
      const row = screen.getByTestId('torrent-card')
      await userEvent.click(within(row).getByRole('button', { name: /^Действия/ }))
      await userEvent.click(screen.getByRole('menuitem', { name: 'Пауза' }))
      await waitFor(() => expect(within(row).getByRole('button', { name: /^Действия/ })).toHaveAttribute('aria-busy', 'true'))
    } finally {
      setViewport(1440)
    }
  })
})
