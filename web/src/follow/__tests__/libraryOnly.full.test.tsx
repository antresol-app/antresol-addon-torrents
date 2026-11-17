// «Мои слежения» in the profile menu: no core entry, the addon's
// profileMenu entry, to its «Мои слежения» page. The core does
// not gate it on libraryOnly; a kid gets no addon entry at all.
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router'
import { resetAddonsForTests } from '@antresol/test-registry'
import type { User } from '@antresol/test-core/api/types'
import { ProfileMenu } from '@antresol/test-core/components/ProfileMenu'
import { session } from '@antresol/test-core/state/session'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const adult: User = { id: 2, name: 'Мама', avatar: 'grad-3', hasPassword: true, isAdmin: false, isKid: false, maxRating: null }
const admin: User = { ...adult, id: 1, name: 'Папа', isAdmin: true }
const kid: User = { ...adult, id: 3, name: 'Коля', isKid: true }

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>
}

async function menu(u: User) {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => (String(input).endsWith('/api/login') ? json(u) : json({}))))
  await session.login(u.id)
  const r = render(
    <MemoryRouter>
      <ProfileMenu user={u} />
      <Where />
    </MemoryRouter>,
  )
  fireEvent.click(screen.getByRole('button', { name: /Профиль|Мама|Папа|Коля/ }))
  return r
}

// the bundle registers once, when its module first loads
beforeAll(async () => {
  session.reset()
  await import('@antresol/test-addon/index')
})
afterAll(() => resetAddonsForTests())
afterEach(() => {
  vi.unstubAllGlobals()
  session.reset()
})

describe('«Мои слежения» is the addon’s entry', () => {
  it('an adult has it, to the addon’s page; the admin not (it has «Управление → Слежение»)', async () => {
    const first = await menu(adult)
    fireEvent.click(screen.getByRole('menuitem', { name: 'Мои слежения' }))
    expect(screen.getByTestId('where')).toHaveTextContent('/addons/torrents/follows')
    first.unmount()
    session.reset()
    await menu(admin)
    expect(screen.queryByRole('menuitem', { name: 'Мои слежения' })).toBeNull()
  })

  it('a kid has none', async () => {
    await menu(kid)
    expect(screen.queryByRole('menuitem', { name: 'Мои слежения' })).toBeNull()
  })
})
