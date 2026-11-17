// The admins' downloads count beside the navigation (navBadges).
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'

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

import { useActiveDownloads } from '../badge/useActiveDownloads'

const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } })
const t = (status: string) => ({ infoHash: status, status })

afterEach(() => vi.unstubAllGlobals())

describe('useActiveDownloads', () => {
  it('counts active downloads from admin.progress', async () => {
    const f = vi.fn(async () => json([t('downloading'), t('seeding'), t('queued')]))
    vi.stubGlobal('fetch', f)
    const { result } = renderHook(() => useActiveDownloads())
    await waitFor(() => expect(result.current).toBe(2))
    expect(String(f.mock.calls[0]![0])).toBe('/api/addons/torrents/x/admin/torrents')
    expect(listened).toEqual(['addon.torrents.admin.progress'])
    act(() => emit!({ type: 'addon.torrents.admin.progress', data: [t('downloading'), t('waiting_space'), t('queued'), t('paused'), t('error')] }))
    expect(result.current).toBe(3)
    // the adults' list is not the download list
    act(() => emit!({ type: 'addon.torrents.progress', data: [] }))
    expect(result.current).toBe(3)
  })
})
