// The addon's registration: admin-only pages and «Управление» tiles, its
// events and strings.
import { afterAll, describe, expect, it } from 'vitest'
// the app's slot registry (a test-only alias of web/vitest.torrents.config.ts)
import { contributionsOf, resetAddonsForTests } from '@antresol/test-registry'

describe('registerAddon("torrents")', () => {
  // the bundle registers once, when its module first loads
  afterAll(() => resetAddonsForTests())

  it('registers admin-only routes and tiles', async () => {
    await import('../index')
    const c = contributionsOf('torrents')
    expect(c?.routes?.map((r) => [r.path, Boolean(r.admin)])).toEqual([
      ['downloads', true],
      ['sources', true],
      ['sources/add', true],
      ['sources/add/:id', true],
      ['sources/:sourceId/edit', true],
      ['settings', true],
      // «Мои слежения» is every adult's, «Слежение» the admins'
      ['follows', false],
      ['manage/follows', true],
      // «Переезд» from Prowlarr, Sonarr, Radarr and qBittorrent
      ['migrate', true],
    ])
    expect(c?.manageTiles?.map((t) => [t.to, t.icon, t.admin])).toEqual([
      ['downloads', 'download', true],
      ['sources', 'plug', true],
      ['settings', 'gear', true],
      ['manage/follows', 'eye', true],
    ])
    // labels and titles are strings keys the core resolves when it draws them
    expect(c?.manageTiles?.map((t) => t.label)).toEqual(['common.downloads', 'manage.section.sources', 'tsettings.title', 'follows.manage'])
    expect(c?.routes?.map((r) => c?.strings?.ru?.[r.title])).toEqual(['Загрузки', 'Источники', 'Добавить источник', 'Добавить источник', 'Источники', 'Настройки загрузок', 'Мои слежения', 'Слежение', 'Переезд: Sonarr, Radarr, qBittorrent'])
    expect(c?.manageTiles?.map((t) => c?.strings?.en?.[t.label])).toEqual(['Downloads', 'Sources', 'Download settings', 'Following'])
    // short names: follow-checked for adults, follow-queued reaches admins as admin.follow-queued
    expect(c?.events).toEqual(['progress', 'source-status', 'follow-checked', 'follow-queued', 'migrate.progress'])
    expect(c?.strings?.en?.['tsettings.title']).toBe('Download settings')
  })

  it('registers «Мои слежения» in the profile menu for adults who are not the admin', async () => {
    await import('../index')
    const p = contributionsOf('torrents')?.profileMenu
    expect(p?.map((e) => [e.label, e.icon, e.to, e.admin])).toEqual([['follows.mine', 'eye', 'follows', undefined]])
    const u = { id: 2, name: 'Мама', avatar: '', hasPassword: false, isKid: false, maxRating: null }
    expect(p?.[0]?.visible?.({ ...u, isAdmin: false })).toBe(true)
    expect(p?.[0]?.visible?.({ ...u, isAdmin: true })).toBe(false)
    expect(contributionsOf('torrents')?.strings?.en?.['follows.mine']).toBe('Following')
  })

  it('registers the title page’s panel, watch way, «Скачать» and the episode player', async () => {
    await import('../index')
    const c = contributionsOf('torrents')
    expect(c?.itemPanels).toHaveLength(1)
    expect(c?.itemPanels?.[0]?.admin).toBeFalsy()
    expect(c?.itemActions?.map((a) => [a.label, a.icon, a.watchWay, a.admin])).toEqual([
      ['watch.now', 'play', true, undefined],
      ['download.title', 'download', undefined, undefined],
      ['vo.find', 'speaker', undefined, undefined],
    ])
    expect(c?.itemActions?.map((a) => c?.strings?.ru?.[a.label])).toEqual(['Смотреть сейчас', 'Скачать', 'Найти озвучку'])
    expect(c?.itemActions?.map((a) => c?.strings?.en?.[a.label])).toEqual(['Watch Now', 'Download', 'Find Voice-Over'])
    expect(c?.episodeActions?.map((a) => [a.kinds, a.admin])).toEqual([[['pinned', 'torrent'], undefined]])
    // its kinds' names and places on the core's scale (library 0, online 20)
    const ea = c?.episodeActions?.[0]
    expect(ea?.order).toEqual({ pinned: 10, torrent: 30 })
    expect(Object.values(ea?.labels ?? {}).map((key) => c?.strings?.ru?.[key])).toEqual(['Закреплённая', 'Торрент'])
    expect(Object.values(ea?.labels ?? {}).map((key) => c?.strings?.en?.[key])).toEqual(['Pinned', 'Torrent'])
    // offered only on a title off the library a profile may stream or download
    const title = { id: 0, kind: 'movie', tmdbId: 42, actions: { play: false, toDevice: false, watchNow: true, toServer: true } } as never
    const library = { id: 5, kind: 'movie', tmdbId: 42, actions: { play: true, toDevice: false, watchNow: true, toServer: true } } as never
    const kidTitle = { id: 0, kind: 'movie', tmdbId: 42, actions: { play: false, toDevice: false, watchNow: false, toServer: false } } as never
    expect(c?.itemActions?.map((a) => [a.visible?.(title), a.visible?.(library), a.visible?.(kidTitle)])).toEqual([
      [true, false, false],
      [true, false, false],
      // «Найти озвучку»: a library film or series only
      [false, true, false],
    ])
  })

  it('registers «Найти озвучку» in the player’s track menu', async () => {
    await import('../index')
    const c = contributionsOf('torrents')
    expect(c?.playerTrackActions?.map((a) => [a.label, a.icon, a.admin])).toEqual([['vo.find', 'speaker', undefined]])
    const a = c!.playerTrackActions![0]!
    expect(a.visible?.({ id: 40, title: 'Миллиарды', kind: 'series' })).toBe(true)
    expect(a.visible?.({ id: 40, title: 'Песня', kind: 'music' as never })).toBe(false)
    // its run returns the sheet the core draws over the player
    expect(a.run({ id: 40, title: 'Миллиарды', kind: 'series' }, () => {})).toMatchObject({ props: { itemId: 40, title: 'Миллиарды' } })
  })

  it('every tracking and title-page slot is an adult’s (none admin-only but the admin pages), and every label and title a strings key', async () => {
    await import('../index')
    const c = contributionsOf('torrents')!
    for (const slot of [c.itemPanels, c.itemActions, c.episodeActions, c.playerTrackActions, c.profileMenu]) {
      expect(slot?.length).toBeGreaterThan(0)
      expect(slot?.every((e) => !e.admin)).toBe(true)
    }
    const labels = [
      ...(c.routes ?? []).map((r) => r.title),
      ...(c.manageTiles ?? []).map((t) => t.label),
      ...(c.itemActions ?? []).map((a) => a.label),
      ...(c.playerTrackActions ?? []).map((a) => a.label),
      ...(c.profileMenu ?? []).map((e) => e.label),
      ...(c.navBadges ?? []).map((b) => b.label),
      ...(c.emptyStateActions ?? []).map((a) => a.label),
    ]
    for (const l of labels) {
      expect(c.strings?.ru?.[l], l).toBeTruthy()
      expect(c.strings?.en?.[l], l).toBeTruthy()
    }
  })

  it('registers the search block and the empty pages\' «Найти и добавить»', async () => {
    await import('../index')
    const c = contributionsOf('torrents')
    expect(c?.searchBlocks).toHaveLength(1)
    expect(c?.searchBlocks?.[0]?.admin).toBeFalsy() // adults' too
    expect(c?.emptyStateActions?.map((a) => [a.scope, a.to, a.label, a.icon, a.admin])).toEqual([
      ['home', '/search', 'find.add', 'search', undefined],
      ['video', '/search?dk=video', 'find.add', 'search', undefined],
      ['music', '/search?dk=music', 'find.add', 'search', undefined],
      ['book', '/search?dk=book', 'find.add', 'search', undefined],
      ['audiobook', '/search?dk=audiobook', 'find.add', 'search', undefined],
      ['comic', '/search?dk=comic', 'find.add', 'search', undefined],
    ])
    expect(c?.strings?.ru?.['find.add']).toBe('Найти и добавить')
  })

  it('registers the input player\'s note, keep action and progress', async () => {
    await import('../index')
    const n = contributionsOf('torrents')?.playerNotes
    expect(n).toHaveLength(1)
    expect(n?.[0]?.admin).toBeFalsy()
    expect(typeof n?.[0]?.actions).toBe('function')
    expect(typeof n?.[0]?.useProgress).toBe('function')
  })

  it('registers the downloads badge, request, stats and adults’ slots (their admin flags)', async () => {
    await import('../index')
    const c = contributionsOf('torrents')
    // the downloads count: admins', to the addon's downloads page
    expect(c?.navBadges?.map((b) => [b.to, b.icon, b.label, b.admin])).toEqual([['downloads', 'download', 'badge.label', true]])
    expect(c?.strings?.ru?.['badge.label']).toBe('Загрузки: {count}')
    expect(c?.strings?.en?.['badge.label']).toBe('Downloads: {count}')
    expect(typeof c?.navBadges?.[0]?.useCount).toBe('function')
    // «Одобрить»'s sheet for every request (no `visible`), the stats block admins' only
    expect(c?.requestActions).toHaveLength(1)
    expect(c?.requestActions?.[0]?.visible).toBeUndefined()
    expect(c?.statsBlocks?.map((b) => b.admin)).toEqual([true])
    // adults' slots stay adults'
    expect([c?.searchBlocks?.[0]?.admin, c?.playerNotes?.[0]?.admin, ...(c?.emptyStateActions ?? []).map((a) => a.admin)].every((a) => !a)).toBe(true)
  })

  it('registers the «Переезд» page and its card, and no import source while the music import waits', async () => {
    await import('../index')
    const c = contributionsOf('torrents')!
    const r = c.routes?.find((x) => x.path === 'migrate')
    expect([r?.admin, r?.title]).toEqual([true, 'migrate.addon.title'])
    expect(typeof r?.element).toBe('function')
    expect(c.migrateSources?.map((e) => [e.id, e.name, e.icon, e.to, e.admin])).toEqual([['torrents', 'migrate.addon.sources', 'arrowRight', 'migrate', true]])
    expect(c.strings?.ru?.['migrate.addon.sources']).toBe('Prowlarr, Sonarr, Radarr, qBittorrent')
    expect(c.strings?.en?.['migrate.addon.title']).toBe('Migration: Sonarr, Radarr, qBittorrent')
    // admins get addon.torrents.admin.migrate.progress (contract «Events»)
    expect(c.events).toContain('migrate.progress')
    expect(c.importSources).toBeUndefined()
  })
})
