// The addon's «Переезд» page (routes 'migrate'): the core's reusable page
// over the addon's own x/admin/migrate… and its progress event.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { MigrateReport } from '@antresol/addon-sdk'
import type { MigrateStatus } from '../types'

type Ev = { type: string; data: unknown }
let emit: ((e: Ev) => void) | null = null
let listened: readonly string[] | undefined
vi.mock('@antresol/addon-sdk', async (orig) => ({
  ...(await orig<typeof import('@antresol/addon-sdk')>()),
  useEvents: (h: (e: Ev) => void, types?: readonly string[]) => {
    emit = (e) => (!types || types.includes(e.type)) && h(e)
    listened = types
  },
}))

import { MigratePageRoute } from '../migrate/MigratePageRoute'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const X = '/api/addons/torrents/x/admin/migrate'

const status: MigrateStatus = {
  sources: [{ id: 'sonarr', name: 'Sonarr', found: true, path: '/import/sonarr/sonarr.db', detail: '1 сериал', error: '' }],
  leftovers: [],
  users: [],
  profiles: [{ id: 1, name: 'Александр' }],
  running: null,
  state: { switchedAt: null },
  pendingFollows: 0,
  libraryReady: { ok: true, reason: '' },
}

const dry = (done: boolean): MigrateReport => ({
  id: 'dry-1',
  sources: ['sonarr'],
  dryRun: true,
  planId: '',
  planHash: 'h',
  startedAt: new Date(Date.now() - 5000).toISOString(),
  // a plan is good for an hour after its check
  finishedAt: done ? new Date().toISOString() : '0001-01-01T00:00:00Z',
  counts: done ? { planned: 1 } : {},
  lines: done ? [{ source: 'sonarr', kind: 'follow', name: 'Миллиарды', status: 'planned', reason: '', ref: '', size: 0 }] : [],
})

afterEach(() => vi.restoreAllMocks())

describe('MigratePageRoute', () => {
  it('checks, then runs the checked plan through x/admin/migrate/run; progress events update the page', async () => {
    let dryDone = false
    let running: string | null = null
    const runs: unknown[] = []
    const fetch = vi.spyOn(globalThis, 'fetch').mockImplementation(async (u, init) => {
      const url = String(u)
      if (url === `${X}/run`) {
        const body = JSON.parse(String(init?.body))
        runs.push(body)
        running = body.dryRun ? 'dry-1' : 'run-1'
        return json({ reportId: running })
      }
      if (url === `${X}/reports`) return json([])
      if (url === `${X}/reports/dry-1`) return json(dry(dryDone))
      if (url.startsWith(`${X}/reports/`)) return json({ ...dry(false), id: 'run-1', dryRun: false })
      if (url === `${X}/state`) return json({ running, libraryReady: { ok: true, reason: '' } })
      if (url === X) return json(status)
      throw new Error(`unexpected ${url}`)
    })
    render(
      <MemoryRouter>
        <MigratePageRoute />
      </MemoryRouter>,
    )
    // the addon's own texts for its services (not the core's catalog)
    expect(await screen.findByText('Сериалы → «Слежение»')).toBeTruthy()
    expect(screen.getByText(/перенесёт раздачи, источники, слежение и прогресс/)).toBeTruthy()
    expect(listened).toEqual(['addon.torrents.admin.migrate.progress'])

    await userEvent.click(screen.getByRole('button', { name: 'Проверить' }))
    await waitFor(() => expect(runs).toHaveLength(1))
    expect(runs[0]).toMatchObject({ sources: ['sonarr'], dryRun: true })
    await screen.findByTestId('migrate-report')

    // the dry run ends; its progress event makes the page read the report again
    dryDone = true
    running = null
    const asked = () => fetch.mock.calls.filter((c) => String(c[0]) === `${X}/reports/dry-1`).length
    const before = asked()
    act(() => emit!({ type: 'addon.torrents.admin.migrate.progress', data: { reportId: 'dry-1' } }))
    expect(await screen.findByText('Миллиарды')).toBeTruthy()
    expect(asked()).toBeGreaterThan(before)

    const runButton = screen.getAllByRole('button', { name: 'Перенести' })[0]!
    await waitFor(() => expect((runButton as HTMLButtonElement).disabled).toBe(false), { timeout: 5000 })
    await userEvent.click(runButton)
    const sheet = await screen.findByRole('alertdialog')
    await userEvent.click(within(sheet).getByRole('button', { name: 'Перенести' }))
    await waitFor(() => expect(runs).toHaveLength(2))
    expect(runs[1]).toMatchObject({ sources: ['sonarr'], dryRun: false, planId: 'dry-1' })
    // every call went to the addon, none to the core's /api/migrate
    expect(fetch.mock.calls.map((c) => String(c[0])).filter((u) => u.startsWith('/api/migrate'))).toEqual([])
  }, 15000)
})
