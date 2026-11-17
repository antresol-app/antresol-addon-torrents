// The downloads screen's cases, cut from manage.full.test.tsx and
// finalFixC.full.test.tsx when the core left downloads to an addon.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { Downloads } from '../pages/Downloads'
import { DownloadRow } from '../pages/DownloadRow'

function json(body: unknown, status = 200) {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

type Handler = (url: string, init?: RequestInit) => Response | undefined
function stubFetch(h: Handler) {
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => h(String(input), init) ?? json({ error: 'нет' }, 404))
  vi.stubGlobal('fetch', f)
  return f
}

afterEach(() => vi.unstubAllGlobals())

describe('Downloads', () => {
  const t: Torrent = {
    infoHash: 'a'.repeat(40), name: 'Dune.2021.mkv', title: 'Дюна', kind: 'movie', mode: 'download', status: 'downloading', size: 100, done: 40,
    downRate: 1000, upRate: 0, uploaded: 0, downloaded: 40, peers: 3, seeds: 1, ratio: 0, itemId: null, addedAt: '2026-10-25T10:00:00Z',
    saveDir: '/t', organized: false,
  }

  it('shows the card and deletes only after a confirmation, with files when asked', async () => {
    const f = stubFetch((u, init) => {
      if (u.endsWith('/api/addons/torrents/x/admin/torrents')) return json([t])
      if (init?.method === 'DELETE') return json(undefined, 204)
      return undefined
    })
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const card = await screen.findByTestId('torrent-card')
    expect(within(card).getByText('Загружается')).toBeInTheDocument()
    expect(within(card).getByText(/^40\s?%$/)).toBeInTheDocument()
    expect(within(card).getByRole('progressbar', { name: 'Прогресс загрузки' })).toHaveAttribute('aria-valuenow', '40')
    await userEvent.click(within(card).getByRole('button', { name: 'Действия: Дюна' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Удалить…' }))
    const sheet = await screen.findByRole('alertdialog')
    expect(f.mock.calls.some(([, i]) => i?.method === 'DELETE')).toBe(false)
    await userEvent.click(within(sheet).getByLabelText('Удалить файлы с диска'))
    await userEvent.click(within(sheet).getByRole('button', { name: 'Удалить' }))
    await waitFor(() => expect(f.mock.calls.some(([u, i]) => i?.method === 'DELETE' && String(u).includes('data=1'))).toBe(true))
  })

  it('offers «Продолжить» on a torrent whose files are missing', async () => {
    stubFetch((u) => (u.endsWith('/api/addons/torrents/x/admin/torrents') ? json([{ ...t, status: 'error', error: 'Файлы раздачи не найдены: /x', dataMissing: true }]) : undefined))
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const card = await screen.findByTestId('torrent-card')
    expect(within(card).getByRole('button', { name: 'Продолжить' })).toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: 'Пауза' })).toBeNull()
  })

  it('«На сервер» on a stream torrent says when it waits for disk space', async () => {
    stubFetch((u, init) => {
      if (u.endsWith('/api/addons/torrents/x/admin/torrents')) return json([{ ...t, mode: 'stream' }])
      if (u.endsWith('/promote') && init?.method === 'POST') return json({ status: 'waiting_space' })
      return undefined
    })
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const card = await screen.findByTestId('torrent-card')
    await userEvent.click(within(card).getByRole('button', { name: 'На сервер' }))
    expect(await within(card).findByText(/Ждёт места на диске/)).toBeInTheDocument()
  })
})

describe('Загрузки shows who added each download', () => {
  it('names the profile', async () => {
    const t: Torrent = {
      infoHash: 'a'.repeat(40), name: 'Dune.mkv', title: 'Дюна', kind: 'movie', mode: 'download', status: 'downloading', size: 100, done: 40,
      downRate: 0, upRate: 0, uploaded: 0, downloaded: 40, peers: 0, seeds: 0, ratio: 0, itemId: null, addedAt: '2026-10-25T10:00:00Z',
      saveDir: '/t', organized: false, addedBy: 2, addedByName: 'Мама',
    }
    stubFetch((u) => {
      if (u.endsWith('/api/addons/torrents/x/admin/torrents')) return json([t])
      if (u.endsWith('/api/disk')) return json({ used: 0, total: 0, level: 'ok', offline: 'Медиадиск недоступен: нет папки /srv/media/torrents' })
      return undefined
    })
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    const cardEl = await screen.findByTestId('torrent-card')
    expect(within(cardEl).getByText('Добавил: Мама')).toBeInTheDocument()
    expect(await screen.findByText(/Медиадиск недоступен/)).toBeInTheDocument()
  })
})

describe('DownloadRow: a seeding download', () => {
  it('says «Раздаётся» (the core\'s status words have no seeding)', () => {
    const t: Torrent = {
      infoHash: 'b'.repeat(40), name: 'Dune.mkv', title: 'Дюна', kind: 'movie', mode: 'download', status: 'seeding', size: 100, done: 100,
      downRate: 0, upRate: 0, uploaded: 0, downloaded: 100, peers: 0, seeds: 0, ratio: 0, itemId: null, addedAt: '2026-10-25T10:00:00Z',
      saveDir: '/t', organized: true,
    }
    render(
      <MemoryRouter>
        <ul>
          <DownloadRow t={t} onChanged={() => {}} />
        </ul>
      </MemoryRouter>,
    )
    expect(screen.getByText(/Раздаётся/)).toBeInTheDocument()
  })
})
