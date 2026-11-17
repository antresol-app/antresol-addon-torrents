// «Настройки загрузок»: the release preferences and «Раздача и место» on the
// addon's x/admin/settings; «Не раздавать» on its own endpoint, locked when
// the server forces it. (Was NoSeedingRow's test: the row lived in the
// core's settings page until the addon took the page.)
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AdminSettings } from '../types'
import { Settings, UploadLimitField } from '../pages/Settings'

const base: AdminSettings = {
  uploadLimited: false,
  uploadLimitKBs: 0,
  streamCacheGB: 50,
  noSeeding: false,
  noSeedingForced: false,
  quality: 1080,
  voice: 'dub',
  voices: ['dub', 'original'],
  maxSizeGB: 0,
  bookLangs: ['ru'],
  preferLossless: true,
  musicFormat: 'lossless',
}
const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const NAME = 'Не раздавать'
const SETTINGS = '/api/addons/torrents/x/admin/settings'

function stub(v: AdminSettings, o: { failNoSeeding?: boolean; incoming?: boolean } = {}) {
  const calls: { method: string; url: string; body: unknown }[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      if (method !== 'GET') calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      if (url.endsWith('/x/admin/settings/no-seeding')) {
        if (o.failNoSeeding) return json({ error: 'нельзя' }, 500)
        if (v.noSeedingForced) return json({ error: 'Задано на сервере' }, 409)
        return new Response(null, { status: 204 })
      }
      if (url.endsWith('/x/admin/settings')) return json(method === 'PUT' ? JSON.parse(String(init?.body)) : v)
      if (url.endsWith('/x/admin/engine')) return json(o.incoming ? { incomingBlocked: true, listenPort: 6881 } : {})
      return json({})
    }),
  )
  return calls
}

// A server with state: «Не раздавать» lives only on its own endpoint; a
// settings PUT stores the rest and answers the whole object as it was when
// the request arrived (so an answer computed before a toggle is stale).
function server(initial: AdminSettings, o: { failSettings?: boolean; holdSettings?: boolean } = {}) {
  const state = { ...initial }
  const puts: unknown[] = []
  const held: (() => void)[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      if (url.endsWith('/x/admin/settings/no-seeding')) {
        state.noSeeding = (JSON.parse(String(init?.body)) as { on: boolean }).on
        return new Response(null, { status: 204 })
      }
      if (url.endsWith('/x/admin/settings')) {
        if (method !== 'PUT') return json(state)
        const body = JSON.parse(String(init?.body)) as Partial<AdminSettings>
        puts.push(body)
        const answer = o.failSettings ? json({ error: 'не сохранилось' }, 400) : json({ ...state, ...body, noSeeding: state.noSeeding })
        if (!o.failSettings) Object.assign(state, body)
        if (o.holdSettings) await new Promise<void>((r) => held.push(r))
        return answer
      }
      return json({})
    }),
  )
  return { state, puts, release: () => held.splice(0).forEach((r) => r()) }
}

describe('«Не раздавать» keeps its own state', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('toggle on, then another setting saved: still on', async () => {
    const srv = server({ ...base })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    await userEvent.click(toggle)
    await waitFor(() => expect(srv.state.noSeeding).toBe(true))
    await userEvent.click(screen.getByRole('radio', { name: '720p' }))
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    await waitFor(() => expect(srv.puts).toHaveLength(1))
    await waitFor(() => expect(screen.getByRole('radio', { name: '720p' })).toBeChecked())
    expect(toggle).toHaveAttribute('aria-checked', 'true')
  })

  it('toggle on while a settings save is in flight: still on after its answer', async () => {
    const srv = server({ ...base }, { holdSettings: true })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    await userEvent.click(screen.getByRole('radio', { name: '720p' }))
    await waitFor(() => expect(srv.puts).toHaveLength(1))
    await userEvent.click(toggle)
    await waitFor(() => expect(srv.state.noSeeding).toBe(true))
    srv.release()
    await waitFor(() => expect(screen.getByText('Изменения сохранены')).toBeInTheDocument())
    expect(toggle).toHaveAttribute('aria-checked', 'true')
  })

  it('toggle on, then a settings save fails: still on', async () => {
    const srv = server({ ...base }, { failSettings: true })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    await userEvent.click(toggle)
    await waitFor(() => expect(srv.state.noSeeding).toBe(true))
    await userEvent.click(screen.getByRole('radio', { name: '720p' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('не сохранилось')
    expect(toggle).toHaveAttribute('aria-checked', 'true')
  })

  it('a settings PUT never carries noSeeding, noSeedingForced or the read-only fields', async () => {
    const srv = server({ ...base, noSeedingForced: true, languages: ['ru', 'en'], outsideFileRoots: 2 })
    render(<Settings />)
    await userEvent.click(await screen.findByRole('radio', { name: '720p' }))
    await waitFor(() => expect(srv.puts).toHaveLength(1))
    for (const k of ['noSeeding', 'noSeedingForced', 'languages', 'outsideFileRoots']) expect(srv.puts[0], k).not.toHaveProperty(k)
  })
})

describe('«Не раздавать»', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('saves on its own endpoint', async () => {
    const calls = stub({ ...base })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    await userEvent.click(toggle)
    await waitFor(() => expect(calls).toEqual([{ method: 'PUT', url: `${SETTINGS}/no-seeding`, body: { on: true } }]))
    expect(toggle).toHaveAttribute('aria-checked', 'true')
  })

  it('shows the stored value', async () => {
    stub({ ...base, noSeeding: true })
    render(<Settings />)
    expect(await screen.findByRole('switch', { name: NAME })).toHaveAttribute('aria-checked', 'true')
  })

  it('goes back and says so when the save fails', async () => {
    stub({ ...base }, { failNoSeeding: true })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    await userEvent.click(toggle)
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'false'))
    expect(await within(toggle.closest('li') as HTMLElement).findByRole('status')).toHaveTextContent('нельзя')
  })

  it('a 409 (forced since the page loaded) locks it on', async () => {
    stub({ ...base })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    vi.unstubAllGlobals()
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'Задано на сервере' }, 409)))
    await userEvent.click(toggle)
    await waitFor(() => expect(toggle).toBeDisabled())
    expect(toggle).toHaveAttribute('aria-checked', 'true')
  })

  it('forced no-seeding is locked', async () => {
    const calls = stub({ ...base, noSeeding: false, noSeedingForced: true })
    render(<Settings />)
    const toggle = await screen.findByRole('switch', { name: NAME })
    // forced means on, whatever the saved setting says
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    expect(toggle).toBeDisabled()
    expect(within(toggle.closest('li') as HTMLElement).getByText('Задано на сервере')).toBeInTheDocument()
    await userEvent.click(toggle)
    await new Promise((r) => setTimeout(r, 0))
    expect(calls).toEqual([])
    expect(toggle).toHaveAttribute('aria-checked', 'true')
  })
})

describe('the other settings', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('a change is saved to x/admin/settings, never with a no-seeding write', async () => {
    const calls = stub({ ...base })
    render(<Settings />)
    await userEvent.click(await screen.findByRole('radio', { name: '720p' }))
    await waitFor(() => expect(calls.length).toBe(1))
    expect(calls[0]!.method).toBe('PUT')
    expect(calls[0]!.url).toBe(SETTINGS)
    expect((calls[0]!.body as AdminSettings).quality).toBe(720)
  })

  it('says when no peer can connect in', async () => {
    stub({ ...base }, { incoming: true })
    render(<Settings />)
    expect(await screen.findByText(/6881/)).toBeInTheDocument()
  })

  it('counts downloads outside the folders of «Файлы»', async () => {
    stub({ ...base, outsideFileRoots: 3 })
    render(<Settings />)
    expect(await screen.findByText(/3 загрузки вне папок «Файлов»/)).toBeInTheDocument()
  })

  it('names the load failure', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: 'нет доступа' }, 403)))
    render(<Settings />)
    expect(await screen.findByRole('alert')).toHaveTextContent('нет доступа')
  })
})

describe('the upload limit field', () => {
  it('can be cleared and retyped; only valid values are saved', async () => {
    const onChange = vi.fn()
    render(<UploadLimitField mbs={2} onChange={onChange} />)
    const input = screen.getByLabelText('Скорость отдачи, МБ/с')
    expect(input).toHaveValue('2')
    await userEvent.clear(input)
    expect(input).toHaveValue('')
    await userEvent.type(input, '0,5')
    expect(input).toHaveValue('0,5')
    expect(onChange).toHaveBeenLastCalledWith(0.5)
    expect(onChange).not.toHaveBeenCalledWith(0)
  })

  it('puts the saved value back when left empty', async () => {
    render(<UploadLimitField mbs={2} onChange={() => {}} />)
    const input = screen.getByLabelText('Скорость отдачи, МБ/с')
    await userEvent.clear(input)
    await userEvent.tab()
    expect(input).toHaveValue('2')
  })
})
