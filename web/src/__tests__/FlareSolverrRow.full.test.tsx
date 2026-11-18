// «FlareSolverr» in «Настройки загрузок»: its own endpoint, saved by its
// button only once the address answers; the server's reason shows under the
// field; with nothing saved the row says which address is in use.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { FlareSolverrRow } from '../pages/Settings'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

function stub(answer: (url: string) => Response) {
  const sent: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith('/x/admin/settings/flaresolverr') && init?.method === 'PUT') {
        const { url } = JSON.parse(String(init.body)) as { url: string }
        sent.push(url)
        return answer(url)
      }
      return json({})
    }),
  )
  return sent
}

describe('FlareSolverrRow', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('saves an address that answers and says so', async () => {
    const sent = stub((url) => json({ url, version: '3.3.21' }))
    render(<FlareSolverrRow saved="" server="" />)
    expect(screen.getByText('Не задан: сайты за защитой Cloudflare не работают.')).toBeInTheDocument()
    const save = screen.getByRole('button', { name: 'Сохранить' })
    expect(save).toBeDisabled()
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес FlareSolverr' }), 'http://flaresolverr:8191{Enter}')
    expect(await screen.findByText('Работает · 3.3.21. Источники с ошибкой проверяются заново.')).toBeInTheDocument()
    expect(sent).toEqual(['http://flaresolverr:8191'])
    expect(save).toBeDisabled()
  })

  it('shows the reason when the address is not FlareSolverr', async () => {
    stub(() => json({ error: 'По этому адресу отвечает не FlareSolverr.' }, 422))
    render(<FlareSolverrRow saved="" server="" />)
    await userEvent.type(screen.getByRole('textbox', { name: 'Адрес FlareSolverr' }), 'http://nas:8080')
    await userEvent.click(screen.getByRole('button', { name: 'Сохранить' }))
    expect(await screen.findByText('По этому адресу отвечает не FlareSolverr.')).toBeInTheDocument()
  })

  it('names the server address while none is saved here', () => {
    stub(() => json({}))
    render(<FlareSolverrRow saved="" server="http://solver:8191" />)
    expect(screen.getByText('Сейчас используется адрес с сервера: http://solver:8191')).toBeInTheDocument()
  })
})
