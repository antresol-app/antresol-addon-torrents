// Item links by kind, «На этом
// устройстве» in the profile menu, empty Music/Books for kids and adults,
// download nouns by kind, the passwordless note on the app-password sheet.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { Downloads } from '../pages/Downloads'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
function stubFetch(h: (url: string, init?: RequestInit) => Response | undefined) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => h(String(input), init) ?? json({ error: 'нет' }, 404)),
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('deleting a download names what stays in the library', () => {
  const base = {
    mode: 'download', size: 1000, done: 1000, downRate: 0, upRate: 0, uploaded: 0, downloaded: 1000, peers: 0, seeds: 0, ratio: 0,
    itemId: null, addedAt: '2026-10-25T10:00:00Z', saveDir: '', organized: true,
  } as const
  it('an album torrent and a direct comic', async () => {
    const list = [
      { ...base, infoHash: 'a'.repeat(40), name: 'Кино — Группа крови', title: 'Группа крови', kind: 'album', status: 'seeding' },
      { ...base, infoHash: 'direct-1', name: 'berserk.cbz', title: 'Берсерк', kind: 'comic', status: 'done', direct: true },
    ] as Torrent[]
    stubFetch((u) => (u.endsWith('/api/addons/torrents/x/admin/torrents') ? json(list) : u.endsWith('/api/disk') ? json({ total: 1, used: 0, free: 1 }) : json({})))
    render(
      <MemoryRouter>
        <Downloads />
      </MemoryRouter>,
    )
    await screen.findAllByTestId('torrent-card')
    fireEvent.click(screen.getByRole('button', { name: 'Действия: Группа крови' }))
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Удалить…' }))
    expect(await screen.findByText('Раздача остановится. Альбом в медиатеке останется, если вы не удалите файлы.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Отмена' }))
    // a direct download has nothing to do but delete: the trash is its action
    fireEvent.click(screen.getByRole('button', { name: 'Удалить «Берсерк»…' }))
    expect(await screen.findByText('Запись пропадёт из загрузок. Манга останется в медиатеке.')).toBeInTheDocument()
  })
})
