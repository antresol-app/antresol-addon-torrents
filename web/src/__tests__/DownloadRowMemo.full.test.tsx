// torrent.progress re-sends the whole list every 2 s: a row whose torrent did
// not change must not render again (122 rows on the owner's server).
import { describe, expect, it, vi } from 'vitest'
import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { Torrent } from '../types'
import { DownloadRow, sameTorrent } from '../pages/DownloadRow'

const renders = vi.hoisted(() => ({ n: 0 }))
vi.mock('../StalledNote', () => ({
  StalledNote: () => {
    renders.n++
    return null
  },
}))

const GB = 1024 ** 3
const tor = (o: Partial<Torrent> = {}): Torrent => ({
  infoHash: 'a'.repeat(40),
  name: 'Dune.2021.2160p',
  title: 'Дюна',
  mode: 'download',
  status: 'downloading',
  size: 30 * GB,
  done: 3 * GB,
  downRate: 0,
  upRate: 0,
  uploaded: 0,
  downloaded: 0,
  peers: 0,
  seeds: 0,
  ratio: 0,
  itemId: null,
  addedAt: '2026-11-01T10:00:00Z',
  saveDir: '/t',
  organized: false,
  kind: 'movie',
  poster: 'p.jpg',
  ...o,
})

describe('a download row renders again only when its torrent changed', () => {
  const onChanged = () => {}
  const show = (t: Torrent) => (
    <MemoryRouter>
      <ul>
        <DownloadRow t={t} onChanged={onChanged} />
      </ul>
    </MemoryRouter>
  )

  it('a fresh copy with the same fields: no render; a changed field: one', () => {
    const r = render(show(tor()))
    const first = renders.n
    expect(first).toBeGreaterThan(0)
    r.rerender(show(tor()))
    expect(renders.n).toBe(first)
    r.rerender(show(tor({ done: 4 * GB })))
    expect(renders.n).toBe(first + 1)
  })

  it('sameTorrent: every field equal, none missing or added', () => {
    expect(sameTorrent(tor(), tor())).toBe(true)
    expect(sameTorrent(tor(), tor({ peers: 3 }))).toBe(false)
    expect(sameTorrent(tor(), tor({ watched: true }))).toBe(false)
    expect(sameTorrent(tor({ watched: undefined }), tor())).toBe(false) // a key present but undefined is still a difference
  })
})
