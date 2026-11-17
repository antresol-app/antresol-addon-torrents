// «Статистика»'s seeding block (statsBlocks, admins): x/admin/stats.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SeedingStats } from '../stats/SeedingStats'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

afterEach(() => vi.unstubAllGlobals())

describe('SeedingStats', () => {
  it('shows what is shared and what was uploaded, with the ratio', async () => {
    const f = vi.fn(async () => json({ count: 3, uploadedTotal: 5 * 1024 ** 3, ratio: 2.34 }))
    vi.stubGlobal('fetch', f)
    render(<SeedingStats />)
    expect(await screen.findByText('3')).toBeInTheDocument()
    expect(screen.getByText('раздаются')).toBeInTheDocument()
    expect(screen.getByText(/^отдано другим · в 2,3 раза больше, чем скачано$/)).toBeInTheDocument()
    expect(String(f.mock.calls[0]![0])).toBe('/api/addons/torrents/x/admin/stats')
  })

  it('no ratio under 1.05; one download is «раздаётся»', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ count: 1, uploadedTotal: 0, ratio: 0.4 })))
    render(<SeedingStats />)
    expect(await screen.findByText('раздаётся')).toBeInTheDocument()
    expect(screen.getByText('отдано другим')).toBeInTheDocument()
  })

  it('renders nothing when the addon does not answer', async () => {
    const f = vi.fn(async () => json({ error: 'нет' }, 503))
    vi.stubGlobal('fetch', f)
    const { container } = render(<SeedingStats />)
    await vi.waitFor(() => expect(f).toHaveBeenCalled())
    await new Promise((r) => setTimeout(r, 0))
    expect(container).toBeEmptyDOMElement()
  })
})
