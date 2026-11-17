import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { MigrateReport } from '@antresol/addon-sdk'
import { MigratePageRoute } from '../migrate/MigratePageRoute'
import type { MigrateStatus } from '../types'

// The torrents addon's instance of the reusable «Переезд» page (its `full`
// part over its own API, x/admin/migrate…).

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })

const status: MigrateStatus = {
  sources: [
    { id: 'sonarr', name: 'Sonarr', found: true, path: '/import/sonarr/sonarr.db', detail: '15 сериалов, 15 отслеживается', error: '' },
    { id: 'qbittorrent', name: 'qBittorrent', found: false, path: '/import/qbittorrent/BT_backup', detail: 'Не найден — смонтируйте /import/qbittorrent/BT_backup', error: '' },
    { id: 'jellyfin', name: 'Jellyfin', found: true, path: '/import/jellyfin/jellyfin.db', detail: '', error: '' },
  ],
  leftovers: [{ source: 'librarr', kind: 'leftover', name: 'Librarr', status: 'warning', reason: 'Не переносится: книги из списков Librarr (2) — добавьте вручную', ref: '', size: 0 }],
  users: [{ source: 'jellyfin', sourceId: '', sourceName: 'Jellyfin', vaultUserId: null, rows: 0, error: 'Не удалось прочитать базу Jellyfin' }],
  profiles: [{ id: 1, name: 'Александр' }],
  running: null,
  state: { switchedAt: null },
  pendingFollows: 0,
  libraryReady: { ok: false, reason: 'Медиатека ещё сканируется — подождите' },
}

afterEach(() => vi.restoreAllMocks())

describe('Migrate', () => {
  const report = (r: Partial<MigrateReport>): MigrateReport => ({
    id: 'r',
    sources: ['qbittorrent'],
    dryRun: false,
    planId: '',
    planHash: '',
    startedAt: '2026-10-25T19:30:00Z',
    finishedAt: '0001-01-01T00:00:00Z',
    counts: {},
    lines: [],
    ...r,
  })

  it('offers to give back the torrents of an interrupted run, not of the running one', async () => {
    const cut = report({ id: '20261025T193000Z-aaaaaa', torrents: 2 })
    const now = report({ id: '20261025T203000Z-bbbbbb', torrents: 1, startedAt: '2026-10-25T20:30:00Z' })
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (u) => {
      const url = String(u)
      if (url.endsWith('/x/admin/migrate/reports')) return json([now, cut])
      if (url.includes('/x/admin/migrate/reports/')) return json(now)
      return json({ ...status, running: now.id, libraryReady: { ok: true, reason: '' } })
    })
    render(
      <MemoryRouter>
        <MigratePageRoute />
      </MemoryRouter>,
    )
    const history = await screen.findByRole('list', { name: 'История переноса' })
    const items = within(history).getAllByRole('listitem')
    expect(items).toHaveLength(2)
    expect(await within(items[1]!).findByRole('button', { name: 'Вернуть раздачи в qBittorrent' })).toBeTruthy()
    expect(within(items[0]!).queryByRole('button', { name: 'Вернуть раздачи в qBittorrent' })).toBeNull()
  })
})

describe('Migrate in the full build', () => {
  it('offers «Перейти на …» while follows from Sonarr and Radarr wait, with the full lead', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (u) =>
      json(String(u).endsWith('/x/admin/migrate/reports') ? [] : { ...status, pendingFollows: 3, libraryReady: { ok: true, reason: '' } }),
    )
    render(
      <MemoryRouter>
        <MigratePageRoute />
      </MemoryRouter>,
    )
    expect(await screen.findByTestId('migrate-switch-card')).toBeTruthy()
    expect(screen.getByRole('button', { name: /^Перейти на / })).toBeTruthy()
    expect(screen.getByText(/перенесёт раздачи, источники, слежение и прогресс/)).toBeTruthy()
    // the full services keep their icons' texts and the note under qBittorrent only when found
    expect(screen.getByText('Сериалы → «Слежение»')).toBeTruthy()
  })
})
