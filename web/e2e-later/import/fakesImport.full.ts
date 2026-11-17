// The playlist import's sign-ins (the full build's «Импорт» services),
// played by page routes for import.full.spec (vault only).
import type { Page } from '@playwright/test'

type Json = Record<string, unknown>

/**
 * «Подключить» Spotify in redirect mode, played by page routes (e2e has no
 * Spotify app): the tile is configured, the sign-in "page" is the callback
 * itself (a fulfilled redirect is not routed again), which lands on «Импорт»
 * with ?job=1 — a job
 * that reads the library, then shows its preview (one followed playlist
 * Spotify would not give).
 */
export async function fakeSpotifyConnect(page: Page) {
  let connected = false
  let reads = 0 // loads of the job after the sign-in: the first says reading
  const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
  const progress = { total: 3, matched: 0, review: 0, notFound: 0, have: 0, queued: 0, downloading: 0, done: 0, failed: 0, skipped: 0 }
  const job = () => ({
    id: 1, service: 'spotify', status: reads > 1 ? 'preview' : 'reading', createdAt: '2026-10-27T10:00:00Z',
    counts: reads > 1 ? { songs: 3, liked: 2, albums: 2, playlists: 1, savedAlbums: 1 } : { songs: 0, liked: 0, albums: 0, playlists: 0, savedAlbums: 0 },
    estimatedBytes: 3 * 30 * (1 << 20), disk: { free: 120 * 2 ** 30, total: 500 * 2 ** 30 }, favorites: true,
    playlists: reads > 1
      ? [{ id: 11, name: 'Дорога', songs: 2, readable: true, selected: true }, { id: 12, name: 'Чужой плейлист', songs: 0, readable: false, selected: false }]
      : [],
    progress, error: '', report: { savedAlbumsMissing: 0, unresolved: 0 },
  })
  await page.route(/\/api\/import\/services$/, async (r) => {
    const res = await r.fetch()
    const body = (await res.json()) as { services: Json[] }
    for (const s of body.services) if (s.id === 'spotify') Object.assign(s, { configured: true, connect: 'redirect', setup: [] })
    return r.fulfill({ response: res, json: body })
  })
  await page.route(/\/api\/import\/connect\/spotify$/, (r) =>
    r.fulfill(json({ authorizeUrl: '/api/import/oauth/spotify/callback?code=c1&state=s1', mode: 'redirect', redirectUri: 'https://vault.example.org/api/import/oauth/spotify/callback' })),
  )
  await page.route(/\/api\/import\/oauth\/spotify\/callback/, (r) => {
    connected = true
    return r.fulfill({ status: 302, headers: { location: '/manage/import?job=1' } })
  })
  await page.route(/\/api\/import\/jobs$/, (r) => {
    if (!connected) return r.fulfill(json({ jobs: [] }))
    reads++
    return r.fulfill(json({ jobs: [job()] }))
  })
  await page.route(/\/api\/import\/jobs\/1$/, (r) => {
    reads++
    return r.fulfill(json(job()))
  })
}

/**
 * «Подключить» Яндекс Музыку, played by page routes (e2e has no Yandex
 * client): the tile is configured, «Подключить» gives a code, the first
 * poll is still waiting, the next one brings the job reading the library,
 * whose next load is its preview.
 */
export async function fakeYandexConnect(page: Page) {
  let polls = 0
  let connected = false
  let reads = 0
  const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
  const progress = { total: 2, matched: 0, review: 0, notFound: 0, have: 0, queued: 0, downloading: 0, done: 0, failed: 0, skipped: 0 }
  const job = () => ({
    id: 2, service: 'yandex', status: reads > 0 ? 'preview' : 'reading', createdAt: '2026-10-27T10:00:00Z',
    counts: reads > 0 ? { songs: 2, liked: 2, albums: 1, playlists: 1, savedAlbums: 1 } : { songs: 0, liked: 0, albums: 0, playlists: 0, savedAlbums: 0 },
    estimatedBytes: 2 * 30 * (1 << 20), disk: { free: 120 * 2 ** 30, total: 500 * 2 ** 30 }, favorites: true,
    playlists: reads > 0 ? [{ id: 21, name: 'Дорога', songs: 2, readable: true, selected: true }] : [],
    progress, error: '', report: { savedAlbumsMissing: 0, unresolved: 0 },
  })
  await page.route(/\/api\/import\/services$/, async (r) => {
    const res = await r.fetch()
    const body = (await res.json()) as { services: Json[] }
    for (const s of body.services) if (s.id === 'yandex') Object.assign(s, { configured: true, connect: 'device', setup: [] })
    return r.fulfill({ response: res, json: body })
  })
  await page.route(/\/api\/import\/connect\/yandex$/, (r) =>
    r.fulfill(json({ pollId: 'p1', userCode: 'ABCD1234', verificationUrl: 'https://ya.ru/device', expiresIn: 300, interval: 5 })),
  )
  await page.route(/\/api\/import\/connect\/yandex\/poll$/, (r) => {
    polls++
    if (polls < 2) return r.fulfill(json({ pending: true }, 202))
    connected = true
    return r.fulfill(json(job(), 201))
  })
  await page.route(/\/api\/import\/jobs$/, (r) => {
    if (!connected) return r.fulfill(json({ jobs: [] }))
    reads++
    return r.fulfill(json({ jobs: [job()] }))
  })
  await page.route(/\/api\/import\/jobs\/2$/, (r) => {
    reads++
    return r.fulfill(json(job()))
  })
  return { polls: () => polls }
}

/**
 * «Подключить» YouTube Music without a public address, played by page
 * routes (e2e has no Google client): the tile is configured in the paste
 * mode, the sign-in tab opens a local page, the pasted loopback address
 * brings the job reading the library, whose next load is its preview.
 */
export async function fakeYouTubeConnect(page: Page) {
  let connected = false
  let reads = 0
  let pasted = ''
  const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
  const progress = { total: 3, matched: 0, review: 0, notFound: 0, have: 0, queued: 0, downloading: 0, done: 0, failed: 0, skipped: 0 }
  const job = () => ({
    id: 3, service: 'youtube', status: reads > 0 ? 'preview' : 'reading', createdAt: '2026-10-27T10:00:00Z',
    counts: reads > 0 ? { songs: 3, liked: 3, albums: 2, playlists: 1, savedAlbums: 0 } : { songs: 0, liked: 0, albums: 0, playlists: 0, savedAlbums: 0 },
    estimatedBytes: 3 * 30 * (1 << 20), disk: { free: 120 * 2 ** 30, total: 500 * 2 ** 30 }, favorites: true,
    playlists: reads > 0 ? [{ id: 31, name: 'Дорога', songs: 2, readable: true, selected: true }] : [],
    progress, error: '', report: { savedAlbumsMissing: 0, unresolved: 0 },
  })
  await page.route(/\/api\/import\/services$/, async (r) => {
    const res = await r.fetch()
    const body = (await res.json()) as { services: Json[] }
    for (const s of body.services) if (s.id === 'youtube') Object.assign(s, { configured: true, connect: 'paste', setup: [] })
    return r.fulfill({ response: res, json: body })
  })
  // the sign-in page stands in for accounts.google.com
  await page.route(/\/api\/import\/connect\/youtube$/, (r) =>
    r.fulfill(json({ authorizeUrl: '/fake-google-sign-in', mode: 'paste', redirectUri: 'http://127.0.0.1:8765/vault-import/youtube' })),
  )
  await page.route(/\/fake-google-sign-in$/, (r) => r.fulfill({ status: 200, contentType: 'text/html', body: '<title>Google</title>Вход' }))
  await page.route(/\/api\/import\/oauth\/youtube\/paste$/, (r) => {
    pasted = (r.request().postDataJSON() as { url: string }).url
    connected = true
    return r.fulfill(json(job(), 201))
  })
  await page.route(/\/api\/import\/jobs$/, (r) => {
    if (!connected) return r.fulfill(json({ jobs: [] }))
    reads++
    return r.fulfill(json({ jobs: [job()] }))
  })
  await page.route(/\/api\/import\/jobs\/3$/, (r) => {
    reads++
    return r.fulfill(json(job()))
  })
  return { pasted: () => pasted }
}

/** «Публичный плейлист по ссылке» of Deezer: the link (no sign-in) → a job reading the playlist → its preview. */
export async function fakeDeezerPublic(page: Page) {
  let linked = false
  let reads = 0
  let sent = ''
  const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
  const progress = { total: 3, matched: 0, review: 0, notFound: 0, have: 0, queued: 0, downloading: 0, done: 0, failed: 0, skipped: 0 }
  const job = () => ({
    id: 4, service: 'deezer', status: reads > 0 ? 'preview' : 'reading', createdAt: '2026-10-27T10:00:00Z',
    counts: reads > 0 ? { songs: 3, liked: 0, albums: 3, playlists: 1, savedAlbums: 0 } : { songs: 0, liked: 0, albums: 0, playlists: 0, savedAlbums: 0 },
    estimatedBytes: 3 * 30 * (1 << 20), disk: { free: 120 * 2 ** 30, total: 500 * 2 ** 30 }, favorites: true,
    playlists: reads > 0 ? [{ id: 41, name: 'Дорога', songs: 3, readable: true, selected: true }] : [],
    progress, error: '', report: { savedAlbumsMissing: 0, unresolved: 0 },
  })
  // the server's link check stands in for api.deezer.com
  await page.route(/\/api\/import\/deezer\/public$/, (r) => {
    sent = (r.request().postDataJSON() as { url: string }).url
    if (sent.includes('404404')) return r.fulfill(json({ error: 'Не найдено' }, 404))
    linked = true
    return r.fulfill(json(job(), 201))
  })
  await page.route(/\/api\/import\/jobs$/, (r) => {
    if (!linked) return r.fulfill(json({ jobs: [] }))
    reads++
    return r.fulfill(json({ jobs: [job()] }))
  })
  await page.route(/\/api\/import\/jobs\/4$/, (r) => {
    reads++
    return r.fulfill(json(job()))
  })
  return { sent: () => sent }
}
