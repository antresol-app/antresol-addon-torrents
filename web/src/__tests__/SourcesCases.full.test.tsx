// The sources screens' cases, cut from manage.full.test.tsx and
// finalFixC.full.test.tsx when the core left sources to an addon.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
import type { CatalogEntry, Source } from '../types'
import { SourceCard } from '../pages/SourceCard'
import { AddSource, EditSource, orderCatalog } from '../pages/AddSource'

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

const entry = (id: string, language = 'en-US', name = id): CatalogEntry => ({ id, name, description: '', language, type: 'public', links: [], custom: false })

describe('catalog order', () => {
  it('puts the popular Russian trackers first, then other Russian sites', () => {
    const out = orderCatalog([entry('1337x'), entry('kinozal', 'ru-RU'), entry('anilibria', 'ru-RU'), entry('rutor', 'ru-RU'), entry('rutracker', 'ru-RU')])
    expect(out.map((e) => e.id)).toEqual(['rutracker', 'rutor', 'kinozal', 'anilibria', '1337x'])
  })
})

describe('SourceCard', () => {
  const src: Source = { id: 1, definitionId: 'rutor', name: 'Rutor', enabled: true, settings: {}, status: 'ok', priority: 0 }
  const noop = () => {}
  it('says «Подключено ✓» or the error in plain words', () => {
    const { rerender } = render(<SourceCard source={src} language="ru-RU" onToggle={noop} onTest={noop} onRemove={noop} />)
    expect(screen.getByText('Подключено ✓')).toBeInTheDocument()
    expect(screen.getByText('Русский')).toBeInTheDocument()
    rerender(<SourceCard source={{ ...src, status: 'error', lastError: 'Сайт не отвечает' }} onToggle={noop} onTest={noop} onRemove={noop} />)
    expect(screen.getByText('Ошибка: Сайт не отвечает')).toBeInTheDocument()
  })

  it('switches a source on and off', async () => {
    const onToggle = vi.fn()
    render(<SourceCard source={src} onToggle={onToggle} onTest={noop} onRemove={noop} />)
    await userEvent.click(screen.getByRole('switch'))
    expect(onToggle).toHaveBeenCalledWith(false)
  })
})

const cookieEntry: CatalogEntry = {
  ...entry('rutracker-org-cookie', 'ru-RU', 'RuTracker.org (cookie)'),
  type: 'semi-private',
  fields: [
    { name: 'cookie', type: 'text', label: 'Cookie', help: 'Как получить:\n1. Войдите на сайт.\n2. Скопируйте cookie.', required: true, advanced: false, secret: true },
    { name: 'sort', type: 'select', label: 'Сортировка на сайте', default: '10', required: false, advanced: true, secret: false, options: [{ value: '10', label: 'по раздающим' }] },
  ],
}

function renderForm() {
  return render(
    <MemoryRouter initialEntries={['/addons/torrents/sources/add/rutracker-org-cookie']}>
      <Routes>
        <Route path="/addons/torrents/sources/add/:id" element={<AddSource />} />
        <Route path="/addons/torrents/sources" element={<p>Список источников</p>} />
      </Routes>
    </MemoryRouter>,
  )
}

describe('AddSource form', () => {
  it('shows only the required field with step-by-step help; the rest is under «Дополнительно»', async () => {
    stubFetch((u) => (u.includes('/api/addons/torrents/x/admin/sources/catalog/') ? json(cookieEntry) : undefined))
    renderForm()
    const cookie = await screen.findByLabelText('Cookie')
    expect(cookie).toBeVisible()
    expect(screen.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['Войдите на сайт.', 'Скопируйте cookie.'])
    expect(screen.getByText('Сортировка на сайте')).not.toBeVisible()
    expect(screen.getByRole('button', { name: 'Подключить' })).toBeDisabled()
  })

  it('«Подключить» → «Подключено ✓»', async () => {
    const f = stubFetch((u, init) => {
      if (u.includes('/api/addons/torrents/x/admin/sources/catalog/')) return json(cookieEntry)
      if (u.endsWith('/api/addons/torrents/x/admin/sources') && init?.method === 'POST') return json({ id: 7, definitionId: cookieEntry.id, name: 'RuTracker', enabled: true, settings: {}, status: 'ok', priority: 0 }, 201)
      return undefined
    })
    renderForm()
    await userEvent.type(await screen.findByLabelText('Cookie'), 'bb_session=1')
    await userEvent.click(screen.getByRole('button', { name: 'Подключить' }))
    expect(await screen.findByText('Подключено ✓')).toBeInTheDocument()
    const post = f.mock.calls.find(([u, i]) => String(u).endsWith('/api/addons/torrents/x/admin/sources') && i?.method === 'POST')!
    expect(JSON.parse(String(post[1]!.body))).toEqual({ definitionId: cookieEntry.id, settings: { cookie: 'bb_session=1', sort: '10' } })
    await userEvent.click(screen.getByRole('button', { name: 'Готово' }))
    expect(screen.getByText('Список источников')).toBeInTheDocument()
  })

  it('shows the server explanation when the site refuses, and a retry updates the same source', async () => {
    let tested = 0
    const f = stubFetch((u, init) => {
      if (u.includes('/api/addons/torrents/x/admin/sources/catalog/')) return json(cookieEntry)
      if (u.endsWith('/api/addons/torrents/x/admin/sources') && init?.method === 'POST')
        return json({ id: 7, definitionId: cookieEntry.id, name: 'RuTracker', enabled: true, settings: {}, status: 'error', lastError: 'Cookie устарел — возьмите новый', priority: 0 }, 201)
      if (u.endsWith('/api/addons/torrents/x/admin/sources/7') && init?.method === 'PATCH') return json({})
      if (u.endsWith('/api/addons/torrents/x/admin/sources/7/test')) {
        tested++
        return json({ id: 7, definitionId: cookieEntry.id, name: 'RuTracker', enabled: true, settings: {}, status: 'ok', priority: 0 })
      }
      return undefined
    })
    renderForm()
    await userEvent.type(await screen.findByLabelText('Cookie'), 'old')
    await userEvent.click(screen.getByRole('button', { name: 'Подключить' }))
    expect(await screen.findByText(/Cookie устарел — возьмите новый. Источник сохранён/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Подключить' }))
    expect(await screen.findByText('Подключено ✓')).toBeInTheDocument()
    expect(tested).toBe(1)
    expect(f.mock.calls.filter(([u, i]) => String(u).endsWith('/api/addons/torrents/x/admin/sources') && i?.method === 'POST')).toHaveLength(1)
  })
})

describe('a saved source can be edited', () => {
  const src: Source = {
    id: 4, definitionId: 'kinozal', name: 'Kinozal', enabled: true, status: 'error', lastError: 'Неверный логин или пароль',
    settings: { username: 'papa', password: '••••••' }, priority: 1,
  }
  it('the card offers «Изменить вход…» next to the error and in the menu', async () => {
    const onEdit = vi.fn()
    render(
      <ul>
        <SourceCard source={src} onToggle={() => {}} onTest={() => {}} onRemove={() => {}} onEdit={onEdit} />
      </ul>,
    )
    await userEvent.click(screen.getByRole('button', { name: 'Изменить вход…' }))
    await userEvent.click(screen.getByRole('button', { name: 'Ещё: Kinozal' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: /Изменить…/ }))
    expect(onEdit).toHaveBeenCalledTimes(2)
  })

  it('the form starts from the saved values and saves into the same source', async () => {
    const f = stubFetch((u, init) => {
      if (u.endsWith('/api/addons/torrents/x/admin/sources') && (init?.method ?? 'GET') === 'GET') return json([src])
      if (u.includes('/api/addons/torrents/x/admin/sources/catalog/kinozal'))
        return json({ id: 'kinozal', name: 'Kinozal', description: '', language: 'ru-RU', type: 'semi-private', links: [], custom: false,
          fields: [
            { name: 'username', type: 'text', label: 'Логин', required: true, advanced: false, secret: false },
            { name: 'password', type: 'password', label: 'Пароль', required: true, advanced: false, secret: true },
          ] })
      if (u.endsWith('/api/addons/torrents/x/admin/sources/4') && init?.method === 'PATCH') return json(src)
      if (u.endsWith('/api/addons/torrents/x/admin/sources/4/test')) return json({ ...src, status: 'ok', lastError: '' })
      return undefined
    })
    render(
      <MemoryRouter initialEntries={['/addons/torrents/sources/4/edit']}>
        <Routes>
          <Route path="/addons/torrents/sources/:sourceId/edit" element={<EditSource />} />
        </Routes>
      </MemoryRouter>,
    )
    expect(await screen.findByRole('heading', { name: 'Изменить: Kinozal' })).toBeInTheDocument()
    const login = screen.getByLabelText('Логин')
    expect(login).toHaveValue('papa')
    await userEvent.clear(login)
    await userEvent.type(login, 'mama')
    await userEvent.click(screen.getByRole('button', { name: 'Сохранить' }))
    expect(await screen.findByText('Подключено ✓')).toBeInTheDocument()
    const patch = f.mock.calls.find(([u, i]) => String(u).endsWith('/api/addons/torrents/x/admin/sources/4') && i?.method === 'PATCH')
    expect(JSON.parse(String(patch![1]!.body))).toEqual({ settings: { username: 'mama', password: '••••••' } })
    expect(f.mock.calls.some(([u, i]) => String(u).endsWith('/api/addons/torrents/x/admin/sources') && i?.method === 'POST')).toBe(false)
  })
})
