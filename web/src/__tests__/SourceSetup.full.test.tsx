// A «Переезд» card's connection form (migrate/SourceSetup.tsx): address and
// API key (qBittorrent: address, login, password), «Подключить» checks them
// on the server, then the card shows the address with «Изменить» and
// «Забыть». The key and the password never come back or show.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { MigrateCtx } from '@antresol/addon-sdk'
import { SourceSetup } from '../migrate/SourceSetup'
import type { MigrateConnect, MigrateStatus } from '../types'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })
const X = '/x/admin/migrate/sources/'

type Source = MigrateStatus['sources'][number] & { connect?: MigrateConnect }

const sonarr = (connect?: MigrateConnect): Source => ({
  id: 'sonarr',
  name: 'Sonarr',
  found: !!connect?.connected,
  path: '/import/sonarr/sonarr.db',
  detail: '',
  error: '',
  connect,
})
const qb = (connect?: MigrateConnect): Source => ({ ...sonarr(connect), id: 'qbittorrent', name: 'qBittorrent' })
const none: MigrateConnect = { kind: 'apikey', url: '', hasSecret: false, connected: false }
const joined: MigrateConnect = { kind: 'apikey', url: 'http://sonarr:8989', hasSecret: true, connected: true }

function ctx(over: Partial<MigrateCtx> = {}): MigrateCtx {
  return {
    status: {} as MigrateStatus,
    running: null,
    ready: true,
    busy: '',
    setBusy: vi.fn(),
    setProblem: vi.fn(),
    reload: vi.fn(),
    check: vi.fn(),
    ...over,
  }
}

interface Call {
  method: string
  path: string
  body: Record<string, unknown> | undefined
}

/** Stubs fetch: records each call and answers with `answer`. */
function stub(answer: (c: Call) => Response) {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input).replace(/^.*\/api\/addons\/torrents/, '')
      const c: Call = { method: init?.method ?? 'GET', path, body: init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined }
      calls.push(c)
      return answer(c)
    }),
  )
  return calls
}

afterEach(() => vi.unstubAllGlobals())

describe('SourceSetup', () => {
  it('draws nothing for a card whose files are mounted', () => {
    const { container } = render(<SourceSetup ctx={ctx()} source={sonarr(undefined)} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('connects with an address and a key, then reads the status again', async () => {
    const calls = stub(() => json({ detail: '312 сериалов, 40 отслеживается', connect: joined }))
    const c = ctx()
    render(<SourceSetup ctx={c} source={sonarr(none)} />)
    const connect = screen.getByRole('button', { name: 'Подключить' })
    expect(connect).toBeDisabled()
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес' }), 'http://sonarr:8989')
    expect(connect).toBeDisabled() // no key yet
    const key = screen.getByLabelText('API-ключ')
    expect(key).toHaveAttribute('type', 'password')
    await userEvent.type(key, 'abc123{Enter}')
    await waitFor(() => expect(c.reload).toHaveBeenCalledTimes(1))
    expect(calls).toEqual([{ method: 'POST', path: `${X}sonarr/connect`, body: { url: 'http://sonarr:8989', apiKey: 'abc123' } }])
  })

  it('says why «Подключить» waits and that the card is selected only after connecting', async () => {
    stub(() => json({}))
    render(<SourceSetup ctx={ctx()} source={sonarr(none)} />)
    expect(screen.getByText('Чтобы отметить Sonarr, сначала подключите его: введите адрес и нажмите «Подключить».')).toBeInTheDocument()
    expect(screen.getByText('Введите адрес Sonarr — тогда «Подключить» станет доступна')).toBeInTheDocument()
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес' }), 'http://sonarr:8989')
    expect(screen.getByText(/^Нужен API-ключ: в Sonarr откройте Settings → General → API Key/)).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('API-ключ'), 'abc')
    expect(screen.queryByText(/^Нужен API-ключ/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Подключить' })).toBeEnabled()
  })

  it('shows the server\'s reason and does not reload when the key is not taken', async () => {
    stub(() => json({ error: 'Sonarr не принял API-ключ — скопируйте его в Sonarr: «Settings → General → API Key»' }, 400))
    const c = ctx()
    render(<SourceSetup ctx={c} source={sonarr(none)} />)
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес' }), 'http://sonarr:8989')
    await userEvent.type(screen.getByLabelText('API-ключ'), 'wrong')
    await userEvent.click(screen.getByRole('button', { name: 'Подключить' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Sonarr не принял API-ключ')
    expect(c.reload).not.toHaveBeenCalled()
    // the typed key stays for a correction, the form stays open
    expect(screen.getByLabelText('API-ключ')).toHaveValue('wrong')
  })

  it('says so when the server cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('network'))))
    render(<SourceSetup ctx={ctx()} source={sonarr(none)} />)
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес' }), 'http://sonarr:8989')
    await userEvent.type(screen.getByLabelText('API-ключ'), 'k')
    await userEvent.click(screen.getByRole('button', { name: 'Подключить' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Нет связи с сервером')
  })

  it('shows the address of a connection and never a key', () => {
    stub(() => json({}))
    const { container } = render(<SourceSetup ctx={ctx()} source={sonarr(joined)} />)
    expect(screen.getByText('Подключён по адресу http://sonarr:8989')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Изменить' })).toBeInTheDocument()
    expect(container.querySelector('input')).toBeNull()
  })

  it('forgets a connection and reads the status again', async () => {
    const calls = stub(() => json({ ok: true }))
    const c = ctx()
    render(<SourceSetup ctx={c} source={sonarr(joined)} />)
    await userEvent.click(screen.getByRole('button', { name: 'Забыть' }))
    await waitFor(() => expect(c.reload).toHaveBeenCalledTimes(1))
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({ method: 'DELETE', path: `${X}sonarr/connect` })
  })

  it('changes a connection: the kept key may stay blank for the same address', async () => {
    const calls = stub(() => json({ detail: '1 сериал', connect: joined }))
    const c = ctx()
    render(<SourceSetup ctx={c} source={sonarr(joined)} />)
    await userEvent.click(screen.getByRole('button', { name: 'Изменить' }))
    const key = screen.getByLabelText('API-ключ')
    expect(key).toHaveValue('')
    expect(key).toHaveAttribute('placeholder', 'Сохранён — можно не вводить')
    const url = screen.getByRole('textbox', { name: 'Адрес' })
    expect(url).toHaveValue('http://sonarr:8989')
    await userEvent.click(screen.getByRole('button', { name: 'Подключить' }))
    await waitFor(() => expect(c.reload).toHaveBeenCalledTimes(1))
    expect(calls[0]?.body).toEqual({ url: 'http://sonarr:8989', apiKey: '' })
  })

  it('wants a new key once the address changes', async () => {
    stub(() => json({}))
    render(<SourceSetup ctx={ctx()} source={sonarr(joined)} />)
    await userEvent.click(screen.getByRole('button', { name: 'Изменить' }))
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес' }), '9')
    expect(screen.getByRole('button', { name: 'Подключить' })).toBeDisabled()
    expect(screen.getByLabelText('API-ключ')).not.toHaveAttribute('placeholder', 'Сохранён — можно не вводить')
  })

  it('cancels a change back to the connection', async () => {
    stub(() => json({}))
    render(<SourceSetup ctx={ctx()} source={sonarr(joined)} />)
    await userEvent.click(screen.getByRole('button', { name: 'Изменить' }))
    await userEvent.click(screen.getByRole('button', { name: 'Отмена' }))
    expect(screen.getByText('Подключён по адресу http://sonarr:8989')).toBeInTheDocument()
  })

  it('asks qBittorrent for a login and a password, not a key', async () => {
    const calls = stub(() => json({ detail: '12 раздач', connect: { kind: 'login', url: 'http://qbittorrent:8080', username: 'admin', hasSecret: true, connected: true } }))
    const c = ctx()
    render(<SourceSetup ctx={c} source={qb({ kind: 'login', url: '', hasSecret: false, connected: false })} />)
    expect(screen.queryByLabelText('API-ключ')).toBeNull()
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес' }), 'http://qbittorrent:8080')
    await userEvent.type(screen.getByRole('textbox', { name: 'Логин' }), 'admin')
    await userEvent.type(screen.getByLabelText('Пароль'), 'p@ss{Enter}')
    await waitFor(() => expect(c.reload).toHaveBeenCalledTimes(1))
    expect(calls).toEqual([{ method: 'POST', path: `${X}qbittorrent/connect`, body: { url: 'http://qbittorrent:8080', username: 'admin', password: 'p@ss' } }])
  })

  it('locks the form while a run or a check is going', () => {
    stub(() => json({}))
    render(<SourceSetup ctx={ctx({ running: 'r1' })} source={sonarr(none)} />)
    expect(screen.getByRole('textbox', { name: 'Адрес' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Подключить' })).toBeDisabled()
  })
})
