// The addon's own server API (its admin routes: the old
// build's shapes under x/admin/…, and the adults' x/discover,
// x/releases, x/acquire…, x/watch…), with the names the pages used for the
// core's routes before they moved.
import { addonApi, ApiError } from '@antresol/addon-sdk'
import { tr } from './i18n'
import type {
  AcquireRequest,
  AcquireResult,
  AdminSettings,
  Candidate,
  CatalogEntry,
  DiscoverKind,
  EngineStatus,
  Follow,
  FollowResolution,
  MigrateConnect,
  MigrateConnectRequest,
  MigrateReport,
  MigrateRunRequest,
  MigrateState,
  MigrateStatus,
  MigrateSwitchPlan,
  ImportedSource,
  ProwlarrLink,
  Pin,
  PinTitle,
  PromoteResult,
  ReleaseFiles,
  ReleaseFilesRequest,
  ReleaseProgress,
  ReleaseSearch,
  Source,
  StreamStatus,
  TitleProgress,
  Torrent,
  VoiceOverDownload,
  VoiceOverPreview,
  VoiceOvers,
} from './types'

export const ID = 'torrents'
const api = addonApi(ID)

const ih = (infoHash: string) => encodeURIComponent(infoHash)
const q = (params: Record<string, string>) => {
  const s = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== '')).toString()
  return s ? `?${s}` : ''
}
/** A query of optional values (the core's qs): undefined and '' are left out, an array repeats its name. */
const qs = (params: Record<string, string | number | boolean | string[] | undefined>) => {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '') continue
    if (Array.isArray(v)) for (const x of v) u.append(k, x)
    else u.append(k, String(v))
  }
  const s = u.toString()
  return s ? `?${s}` : ''
}

export const getTorrents = (signal?: AbortSignal) => api.get<Torrent[]>('admin/torrents', signal)
export const pauseTorrent = (infoHash: string) => api.post<void>(`admin/torrents/${ih(infoHash)}/pause`)
/** Hides the note of the automatic switch to another release. */
export const dismissTorrentNote = (infoHash: string) => api.post<void>(`admin/torrents/${ih(infoHash)}/dismiss-note`)
export const resumeTorrent = (infoHash: string) => api.post<void>(`admin/torrents/${ih(infoHash)}/resume`)
export const organizeTorrent = (infoHash: string) => api.post<void>(`admin/torrents/${ih(infoHash)}/organize`)
/** Organize without the other-album check («Разложить всё равно»). */
export const forceOrganizeTorrent = (infoHash: string) => api.post<void>(`admin/torrents/${ih(infoHash)}/organize-force`)
/** Keeps a «Смотреть сейчас» download on the server; status is waiting_space on a full disk. */
export const promoteTorrent = (infoHash: string, force = false) =>
  api.post<PromoteResult>(`admin/torrents/${ih(infoHash)}/promote`, force ? { force } : undefined)
export const deleteTorrent = (infoHash: string, deleteData: boolean) =>
  api.del<void>(`admin/torrents/${ih(infoHash)}${q({ data: deleteData ? '1' : '0' })}`)
export const getEngine = (signal?: AbortSignal) => api.get<EngineStatus>('admin/engine', signal)

export const getSources = (signal?: AbortSignal) => api.get<Source[]>('admin/sources', signal)
export const getCatalog = (query: string, signal?: AbortSignal) => api.get<CatalogEntry[]>(`admin/sources/catalog${q({ q: query })}`, signal)
export const getCatalogEntry = (id: string, signal?: AbortSignal) =>
  api.get<CatalogEntry>(`admin/sources/catalog/${encodeURIComponent(id)}`, signal)
/** Saved and tested at once: the outcome is in status/lastError. */
export const addSource = (definitionId: string, settings: Record<string, string>) => api.post<Source>('admin/sources', { definitionId, settings })
export const updateSource = (id: number, patch: { name?: string; enabled?: boolean; settings?: Record<string, string>; priority?: number }) =>
  api.patch<Source>(`admin/sources/${id}`, patch)
export const testSource = (id: number) => api.post<Source>(`admin/sources/${id}/test`)
export const deleteSource = (id: number) => api.del<void>(`admin/sources/${id}`)
export const importProwlarr = (path: string) =>
  api.post<{ imported: ImportedSource[] }>('admin/sources/import-prowlarr', path ? { path } : {})

export const getSettings = (signal?: AbortSignal) => api.get<AdminSettings>('admin/settings', signal)
/** What a settings PUT carries: not «Не раздавать» (saveNoSeeding writes it) and not what only the server reports. */
export type SettingsBody = Omit<AdminSettings, 'noSeeding' | 'noSeedingForced' | 'languages' | 'outsideFileRoots' | 'flareSolverrUrl' | 'flareSolverrServer'>
/** Everything but «Не раздавать»; the answer is the whole object, whose no-seeding fields the page ignores. */
export const saveSettings = (s: AdminSettings) => {
  const { noSeeding: _n, noSeedingForced: _f, languages: _l, outsideFileRoots: _o, flareSolverrUrl: _u, flareSolverrServer: _s, ...body } = s
  return api.put<AdminSettings>('admin/settings', body satisfies SettingsBody)
}
/** «Не раздавать»: applied by the server at once; refused (409) while forced by the server. */
export const saveNoSeeding = (on: boolean) => api.put<void>('admin/settings/no-seeding', { on })
/** FlareSolverr's address: saved only once it answers as FlareSolverr (422 with the reason otherwise); "" clears it. */
export const saveFlareSolverr = (url: string) => api.put<{ url: string; version?: string }>('admin/settings/flaresolverr', { url })

// --- «Найти и добавить» and «Смотреть сейчас» (adults) ---

/** x/discover: TMDB titles (video, no kind sent), MusicBrainz albums, Open Library books, or a manga query. */
export const discover = (query: string, kind: DiscoverKind = 'video', signal?: AbortSignal) =>
  api.get<Candidate[]>(`discover${qs({ q: query, kind: kind === 'video' ? undefined : kind })}`, signal)

export interface ReleasesQuery {
  kind: string
  /** Movies and series. */
  tmdbId?: number
  /** Music and books: the catalog id, the album or book and its artist or author. */
  extId?: string
  title?: string
  artist?: string
  /** Music: the album's year (releases of another year are rejected) and its other names. */
  year?: number
  alias?: string[]
  season?: number
  episode?: number
}
export const getReleases = (rq: ReleasesQuery, signal?: AbortSignal) => api.get<ReleaseSearch>(`releases${qs({ ...rq })}`, signal)

type ReleaseLine = ReleaseProgress & { done?: boolean; error?: string }

/**
 * The releases as the sources answer (x/releases?stream=1, NDJSON):
 * `onProgress` gets the releases of the sources that answered so far and
 * the sources still pending; resolves with the final line's set. A line
 * without `pending` is a whole answer (not the stream). A stream that ends
 * without its final line was cut.
 */
export async function streamReleases(rq: ReleasesQuery, onProgress: (p: ReleaseProgress) => void, signal?: AbortSignal): Promise<ReleaseSearch> {
  let final: ReleaseSearch | null = null
  await api.stream<ReleaseLine>(
    `releases${qs({ ...rq, stream: '1' })}`,
    (p) => {
      if (final) return
      if (p.error) throw new ApiError(500, p.error)
      if (p.done || !Array.isArray(p.pending)) final = p
      else onProgress(p)
    },
    signal,
  )
  if (!final) throw new ApiError(0, tr('comp.release.searchFailed'))
  return final
}

/** «Смотреть сейчас» / «На сервер»: the server picks the release unless the request names one. */
export const acquire = (req: AcquireRequest) => api.post<AcquireResult>('acquire', req)
/** A title's download progress for the download button's ring; null (204) when nothing is downloading. */
export const getTitleProgress = async (tq: { kind: string; tmdbId: number; season?: number }, signal?: AbortSignal): Promise<TitleProgress | null> =>
  (await api.get<TitleProgress | undefined>(`acquire/progress${qs({ ...tq })}`, signal)) ?? null

type TitleQuery = { kind: string; tmdbId: number; season?: number }
interface Asked {
  tq: TitleQuery
  signal?: AbortSignal
  resolve: (p: TitleProgress | null) => void
  reject: (e: unknown) => void
}
let asked: Asked[] = []
/** The most titles one batch request names (the server's limit). */
const BATCH_MAX = 100
const titleKey = (tq: TitleQuery) => `${tq.kind}:${tq.tmdbId}${tq.season ? `:${tq.season}` : ''}`

async function askBatch(batch: Asked[]) {
  const keys = [...new Set(batch.map((a) => titleKey(a.tq)))]
  try {
    const got = (await api.get<Record<string, TitleProgress> | undefined>(`acquire/progress/batch?${keys.map((k) => `t=${k}`).join('&')}`)) ?? {}
    for (const a of batch) a.resolve(got[titleKey(a.tq)] ?? null)
  } catch (e) {
    for (const a of batch) a.reject(e)
  }
}

function flushAsked() {
  const live = asked.filter((a) => {
    if (!a.signal?.aborted) return true
    a.reject(new DOMException('Aborted', 'AbortError'))
    return false
  })
  asked = []
  const [one] = live
  if (live.length === 1 && one) {
    getTitleProgress(one.tq, one.signal).then(one.resolve, one.reject)
    return
  }
  for (let i = 0; i < live.length; i += BATCH_MAX) void askBatch(live.slice(i, i + BATCH_MAX))
}

/**
 * getTitleProgress, with the titles asked in the same moment (the cards of
 * a search drawn at once) sent as one batch request; one alone goes as it is.
 */
export function getTitleProgressBatched(tq: TitleQuery, signal?: AbortSignal): Promise<TitleProgress | null> {
  return new Promise((resolve, reject) => {
    if (asked.length === 0) void Promise.resolve().then(flushAsked)
    asked.push({ tq, signal, resolve, reject })
  })
}

/** «Отменить загрузку»: stops and deletes a title's unfinished downloads. */
export const cancelTitleDownload = (tq: { kind: string; tmdbId: number; season?: number }) =>
  api.post<{ cancelled: number }>('acquire/cancel', { kind: tq.kind, tmdbId: tq.tmdbId, season: tq.season ?? 0 })
/** The track step's file list (admins); an answer without a JSON error (a proxy's 502/504 page) reads as «Не удалось получить список файлов». */
// (addonApi.post takes no signal: the caller ignores a late answer)
export async function getReleaseFiles(req: ReleaseFilesRequest): Promise<ReleaseFiles> {
  try {
    return await api.post<ReleaseFiles>('admin/acquire/files', req)
  } catch (e) {
    // the core's words for an error without a JSON body (common.genericError, the same text here)
    if (e instanceof ApiError && e.status !== 0 && e.message === tr('common.genericError')) throw new ApiError(e.status, tr('app.api.filesFailed'))
    throw e
  }
}
/** «Оставить на сервере» (any adult): a «Смотреть сейчас» download goes to the library; status is waiting_space on a full disk. */
export const promote = (infoHash: string) => api.post<PromoteResult>(`torrents/${ih(infoHash)}/promote`)
/** A «Смотреть сейчас» download's state, with the buffer of file `fileIndex`. */
export const getTorrentStatus = (infoHash: string, signal?: AbortSignal, fileIndex?: number) =>
  api.get<StreamStatus>(`torrents/${ih(infoHash)}/status${fileIndex !== undefined ? `?file=${fileIndex}` : ''}`, signal)
/** «Смотреть сейчас»: file `idx` of the download becomes an input of the core's player (/play/input/<inputId>). */
export const watchNow = (infoHash: string, idx: number) => api.post<{ inputId: string }>(`watch/${ih(infoHash)}/${idx}`)

/** The seeding block of «Статистика» (admins). */
export interface SeedingStats {
  count: number
  uploadedTotal: number
  ratio: number
}
export const getSeedingStats = (signal?: AbortSignal) => api.get<SeedingStats>('admin/stats', signal)

// --- «Следить» (adults; x/follow…) ---

/** Every follow the profile may see (an adult: their own; the admin: all). */
export const getFollows = (signal?: AbortSignal) => api.get<{ items: Follow[] }>('follow', signal).then((r) => r.items)
/** The follow of a title, null when nobody follows it (404). */
export async function getFollowByTMDB(kind: 'series' | 'movie', tmdbId: number, signal?: AbortSignal): Promise<Follow | null> {
  try {
    return await api.get<Follow>(`follow/by-tmdb${q({ kind, tmdbId: String(tmdbId) })}`, signal)
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null
    throw e
  }
}
/** Starts following; the server checks a new follow by itself (follow-checked). */
export const follow = (kind: 'series' | 'movie', tmdbId: number, resolution?: FollowResolution) =>
  api.post<Follow>('follow', resolution === undefined ? { kind, tmdbId } : { kind, tmdbId, resolution })
export const unfollow = (id: number) => api.del<void>(`follow/${id}`)
export const setFollowActive = (id: number, active: boolean, resolution?: FollowResolution) =>
  api.patch<Follow>(`follow/${id}`, resolution === undefined ? { active } : { active, resolution })
/** 202: the check runs in the background; its end comes as follow-checked. */
export const checkFollow = (id: number) => api.post<{ started: boolean }>(`follow/${id}/check`)

// --- the household's pinned release of a title (adults; x/pinned/{kind}/{tmdbId}…) ---

type PinKind = 'series' | 'movie'
const pinPath = (kind: PinKind, tmdbId: number) => `pinned/${kind}/${tmdbId}`

export const getPin = (kind: PinKind, tmdbId: number, signal?: AbortSignal) =>
  api.get<{ pin: Pin | null }>(pinPath(kind, tmdbId), signal).then((r) => r.pin ?? null)
/** «Закрепить» a release of the search (its download link). */
export const pinRelease = (kind: PinKind, tmdbId: number, r: { sourceId: number; link: string; details?: string; releaseTitle: string }, title: PinTitle = {}) =>
  api.post<{ pin: Pin }>(pinPath(kind, tmdbId), { ...r, ...title }).then((x) => x.pin)

/** The largest own .torrent file the server takes (its own check says the same words). */
export const MAX_OWN_TORRENT = 8 << 20

/** A file's bytes as base64 (the proxy takes JSON only). */
async function base64Of(file: Blob): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer())
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  return btoa(bin)
}

/**
 * «Добавить свою раздачу»: a magnet or a topic URL, or a .torrent file sent
 * as `{file: base64, name, poster}` (`name` and `poster` are the title's). A file over 8 MiB is refused here, before
 * anything is read or sent.
 */
export async function pinOwn(kind: PinKind, tmdbId: number, own: { input: string } | { file: File }, title: PinTitle = {}): Promise<Pin> {
  if ('file' in own) {
    if (own.file.size > MAX_OWN_TORRENT) throw new ApiError(400, tr('pin.ownTooBig'))
    const file = await base64Of(own.file)
    return api.post<{ pin: Pin }>(`${pinPath(kind, tmdbId)}/own`, { file, ...title }).then((x) => x.pin)
  }
  return api.post<{ pin: Pin }>(`${pinPath(kind, tmdbId)}/own`, { input: own.input, ...title }).then((x) => x.pin)
}
export const unpin = (kind: PinKind, tmdbId: number) => api.del<void>(pinPath(kind, tmdbId))
/** «Обновить сейчас»: the topic is fetched again; changed: a re-upload became the pin. */
export const refreshPin = (kind: PinKind, tmdbId: number) => api.post<{ pin: Pin; changed: boolean }>(`${pinPath(kind, tmdbId)}/refresh`)
/** «Заменить»: the suggested release becomes the pin. */
export const acceptPinSuggestion = (kind: PinKind, tmdbId: number) =>
  api.post<{ pin: Pin }>(`${pinPath(kind, tmdbId)}/accept-suggestion`).then((x) => x.pin)
/** «Смотреть» an episode (a film: none) from the pin: the file becomes an input of the core's player (/play/input/<inputId>). */
export const playPinned = (kind: PinKind, tmdbId: number, season?: number, episode?: number) =>
  api.post<{ inputId: string }>(`${pinPath(kind, tmdbId)}/play`, kind === 'series' ? { season, episode } : {})
/** «Скачать серию»: episodes of the pin download to the server (409: none of them in the pin). */
export const downloadPinnedEpisodes = (kind: PinKind, tmdbId: number, season: number, episodes: number[]) =>
  api.post<AcquireResult>(`${pinPath(kind, tmdbId)}/download`, { season, episodes })

// --- «Найти озвучку…» (adults; x/items/{id}/voiceovers…): one search of the sources per call ---

export const findVoiceOvers = (itemId: number, signal?: AbortSignal) => api.get<VoiceOvers>(`items/${itemId}/voiceovers`, signal)
/** A candidate's file list (at most five a search): the episodes it covers. */
export const previewVoiceOver = (itemId: number, id: string) => api.post<VoiceOverPreview>(`items/${itemId}/voiceovers/files`, { id })
export const downloadVoiceOver = (itemId: number, id: string) => api.post<{ infoHash: string; existing?: boolean }>(`items/${itemId}/voiceovers`, { id })
export const voiceOverDownloads = (itemId: number, signal?: AbortSignal) => api.get<VoiceOverDownload[]>(`items/${itemId}/voiceovers/downloads`, signal)

// --- «Переезд» from Prowlarr, Sonarr, Radarr and qBittorrent (admins; x/admin/migrate…) ---

const report = (id: string) => `admin/migrate/reports/${encodeURIComponent(id)}`
/** The old services found under /import, their users and what does not move. */
export const migrateStatus = () => api.get<MigrateStatus>('admin/migrate')
/** What changes during a run (reads no old database). */
export const migrateState = () => api.get<MigrateState>('admin/migrate/state')
/** A dry run, or the real run of a checked plan (planId); progress comes as admin.migrate.progress. */
export const migrateRun = (req: MigrateRunRequest) => api.post<{ reportId: string }>('admin/migrate/run', req)
export const migrateReports = () => api.get<MigrateReport[]>('admin/migrate/reports')
export const migrateReport = (id: string) => api.get<MigrateReport>(report(id))
/** «Вернуть раздачи в qBittorrent»: a rollback of a run, as a report of its own. */
export const migrateRollback = (id: string) => api.post<{ reportId: string }>(`${report(id)}/rollback`)
/** What «Перейти на …» would turn on. */
export const migrateSwitchPreview = () => api.get<MigrateSwitchPlan>('admin/migrate/switch')
/** Turns on the taken-over follows; 409 when the count changed since the preview. */
export const migrateSwitch = (req: { confirm: number; arrStopped: boolean }) => api.post<{ activated: number }>('admin/migrate/switch', req)
/** «Подключить» a service over its API: kept (in the addon's memory) only when it answers; the key and the password are never answered back. */
export const migrateConnect = (id: string, req: MigrateConnectRequest) =>
  api.post<{ detail: string; connect: MigrateConnect }>(`admin/migrate/sources/${encodeURIComponent(id)}/connect`, req)
/** How Prowlarr's tracker list is reached now (reads no database). */
export const getProwlarrLink = (signal?: AbortSignal) => api.get<{ link: ProwlarrLink }>('admin/migrate/prowlarr', signal)
/** «Забыть»: drops the kept connection. */
export const migrateForget = (id: string) => api.del<void>(`admin/migrate/sources/${encodeURIComponent(id)}/connect`)
