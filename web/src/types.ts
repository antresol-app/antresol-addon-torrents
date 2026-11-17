// The addon's API types (its own routes under x/admin/…, the shapes the
// old build's core answered). Copied from the core's
// api/types.ts when the pages moved; the core no longer has them.

import type { MigrateApi, MigrateReport as CoreMigrateReport } from '@antresol/addon-sdk'

export type TorrentStatus = 'queued' | 'downloading' | 'seeding' | 'paused' | 'waiting_space' | 'error' | (string & {})

/**
 * Why a running download makes no headway (torrent.Status.Stalled): `no_peers`
 * — nobody to download from for a while (a magnet then has no metadata either);
 * `no_metadata` — a magnet has peers but still no metadata. Absent: nothing to say.
 */
export type TorrentStalled = 'no_peers' | 'no_metadata' | (string & {})

/**
 * What a short probe of the swarm found when the release was picked
 * (torrent.Status.Liveness): advisory only — a seeder may still connect in later.
 * `unseen`: no tracker and no DHT node knew a peer. Absent: never probed.
 */
export type TorrentLiveness = 'checking' | 'alive' | 'unseen' | (string & {})

/** torrent.Status.TrackerErrorKind: what a tracker's refusal means. */
export type TrackerErrorKind = 'bad_passkey' | 'not_registered' | 'rejected' | (string & {})

/** Why a download makes no headway, as far as the server knows (StalledNote). */
export interface TorrentHint {
  stalled?: TorrentStalled
  liveness?: TorrentLiveness
  /** The tracker's own words, passkeys hidden. */
  trackerError?: string
  trackerErrorKind?: TrackerErrorKind
}

/**
 * acquire.TorrentView.AutoReplace: `switched` — Antresol started this download
 * itself in place of a release nobody seeded; `none_alive` — no peer and no live
 * alternative was found.
 */
export type AutoReplaceNote = 'switched' | 'none_alive' | (string & {})

/** GET x/admin/torrents: torrent.Status plus the library side (acquire.TorrentView). */
export interface Torrent {
  infoHash: string
  name: string
  mode: 'stream' | 'download' | 'seed' | (string & {})
  status: TorrentStatus
  size: number
  done: number
  downRate: number
  upRate: number
  uploaded: number
  downloaded: number
  peers: number
  seeds: number
  ratio: number
  itemId: number | null
  /** RFC 3339. */
  addedAt: string
  completedAt?: string
  saveDir: string
  error?: string
  kind?: 'movie' | 'series' | (string & {})
  /** The TMDB title it was added for. */
  title?: string
  organized: boolean
  organizeError?: string
  /** All its files were gone at start: «Продолжить» downloads them again. */
  dataMissing?: boolean
  /** Profile id that added it. */
  addedBy?: number
  /** Profile name that added it (admin: «Добавил: …»). */
  addedByName?: string
  /** What organize did besides placing files, e.g. «Уже есть в медиатеке». */
  note?: string
  /** A direct download of an OPDS source (infoHash «direct-…»): status queued, downloading, done or error; no pause. */
  direct?: boolean
  /** Files chosen in the track step and files in the release (equal = the whole release). */
  selectedFiles?: number
  totalFiles?: number
  /** Music with a partial selection: the songs chosen and the release's songs (covers and CUE sheets left out). */
  selectedTracks?: number
  totalTracks?: number
  /** The cover, a file of the server's image cache (served by /api/images/{poster}); absent = none known. */
  poster?: string
  /** GET x/admin/torrents only: this profile finished the movie or every episode of the series (absent: other kinds, not in the library). */
  watched?: boolean
  stalled?: TorrentStalled
  /** See TorrentHint. */
  liveness?: TorrentLiveness
  trackerError?: string
  trackerErrorKind?: TrackerErrorKind
  /** The movie or series it was added for (and its season; absent = all or not a TMDB title). */
  tmdbId?: number
  season?: number
  autoReplace?: AutoReplaceNote

  /** Finished and out of the swarm: seeding is off. */
  parked?: boolean
}

/** POST x/admin/torrents/{ih}/promote. */
export interface PromoteResult {
  status: TorrentStatus
}

/** A configured torrent source (indexer.Source; secrets are redacted). */
export interface Source {
  id: number
  definitionId: string
  name: string
  enabled: boolean
  settings: Record<string, string>
  status: 'ok' | 'error' | 'unknown' | (string & {})
  lastError?: string
  /** RFC 3339. */
  lastChecked?: string
  /** Lower = preferred. */
  priority: number
  /** This source is the backup way into that one (the same site signed in another way): searched only when it fails, shown inside it. */
  fallbackFor?: number
}

export interface CatalogOption {
  value: string
  label: string
}

/** One form field of a tracker definition. */
export interface CatalogField {
  name: string
  type: string
  label: string
  default?: string
  help?: string
  required: boolean
  advanced: boolean
  secret: boolean
  options?: CatalogOption[]
}

/** A tracker definition from the catalog (fields only in GET x/admin/sources/catalog/{id}). */
export interface CatalogEntry {
  id: string
  name: string
  description: string
  language: string
  type: 'public' | 'semi-private' | 'private' | (string & {})
  links: string[]
  custom: boolean
  fields?: CatalogField[]
}

export interface ImportedSource {
  name: string
  definitionId?: string
  source?: Source
  reason?: string
}

export type LangCode = 'ru' | 'en' | 'uk' | 'de' | 'fr' | 'es' | 'it' | 'pt' | 'pl' | 'ja' | 'zh' | 'ko'
export type Voice = 'dub' | 'original'
export type MusicFormat = 'lossless' | 'mp3' | 'any'
export type Mp3Bitrate = '320' | 'v0' | '256' | 'any'

/** GET/PUT x/admin/settings: downloads, seeding and the release-search preferences. */
export interface AdminSettings {
  uploadLimited: boolean
  uploadLimitKBs: number
  streamCacheGB: number
  /** «Не раздавать» (read-only here: x/admin/settings/no-seeding writes it). */
  noSeeding: boolean
  /** Forced by the server (ANTRESOL_TORRENTS_NO_SEEDING=on): the toggle is locked. */
  noSeedingForced: boolean
  quality: 720 | 1080 | 2160
  /** The first of `voices` (older clients). */
  voice: Voice
  voices?: Voice[]
  maxSizeGB: number
  bookLangs?: LangCode[]
  audiobookLangs?: LangCode[]
  comicLangs?: LangCode[]
  preferLossless?: boolean
  musicFormat?: MusicFormat
  mp3Bitrate?: Mp3Bitrate
  /** The profile's languages, the order «Мои языки» has (the languages a release may be in). */
  languages?: LangCode[]
  /** Downloads whose folder is outside every folder of «Файлы». */
  outsideFileRoots?: number
}

/** GET x/admin/engine. */
export interface EngineStatus {
  /** No peer has ever connected in: the port is probably closed. */
  incomingBlocked?: boolean
  listenPort?: number
}

// --- «Найти и добавить», releases, «Смотреть сейчас» (the adults' routes,
// x/discover, x/releases, x/acquire…, x/torrents/{ih}/status). Copied
// from the core's api/types.ts when find-and-add moved into the addon.

/** The catalog «Найти и добавить» searches (x/discover?kind=; video sends none). */
export type DiscoverKind = 'video' | 'music' | 'book' | 'audiobook' | 'comic'

/** A title of x/discover that is not on the server yet (or an album/book that is). */
export interface Candidate {
  tmdbId: number
  /** MusicBrainz release group or Open Library work (music, books). */
  extId?: string
  kind: 'movie' | 'series' | 'music' | 'book' | 'audiobook' | 'comic' | (string & {})
  title: string
  /** The artist (music) or the first author (books). */
  subtitle?: string
  year: number
  poster: string
  overview: string
  inLibrary: boolean
  libraryItemId: number | null
  /** Music: other names of the album (the MusicBrainz disambiguation, «Blue Album»). */
  aliases?: string[]
  /** Music found by a song title: the song of the query the album holds («Sad But True» on «Metallica»). */
  matchedTrack?: string
  /** Music of the artist the query names («metallica» → their albums, listed first): that artist. */
  byArtist?: string
}

export interface Release {
  title: string
  details?: string
  link?: string
  magnet?: string
  infoHash?: string
  size: number
  seeders: number
  leechers: number
  publishDate?: string
  freeleech?: boolean
  sourceId: number
  sourceName: string
  resolution?: number
  source?: string
  codec?: string
  hdr?: boolean
  audio?: string[]
  /** Subtitle languages (ru, en, uk, ...) or ["sub"] when named without one; absent = none. */
  subs?: string[]
  /** Voice-over studios named by the release (LostFilm, Кубик в Кубе), in order of appearance. */
  studios?: string[]
  season?: number
  episode?: number
  episodeEnd?: number
  seasonPack?: boolean
  score: number
  excluded?: boolean
  reasons: string[]
  /** A file of an OPDS catalog, not a download. */
  direct?: boolean
  /** Books and audiobooks: the language (ru, en, ja); absent = unknown. */
  lang?: string
  /** Music: the format badge («FLAC», «MP3 320», «MP3 V0»); absent = not named. */
  audioFormat?: string
  /** Music: another format than «Формат загрузки» — listed last, never picked by itself. */
  formatMismatch?: boolean
}

/** GET x/releases: the releases of a title, best first. */
export interface ReleaseSearch {
  releases: Release[]
  rejected: Release[]
  failed: { sourceId: number; name: string; message: string }[]
}

/** A line of GET x/releases?stream=1: the releases so far and the sources still searched. */
export interface ReleaseProgress extends ReleaseSearch {
  pending: { sourceId: number; name: string }[]
}

/** POST x/admin/acquire/files: a release's file list for the track step. */
export interface ReleaseFilesRequest {
  sourceId?: number
  releaseLink: string
  title: string
  artist: string
  year: number
  aliases: string[]
  direct?: boolean
  /** The song the person searched for: its track comes back `matched` («Выбрать треки…» checks only it). */
  song?: string
}

export interface ReleaseFile {
  index: number
  path: string
  size: number
  /** A song, or anything else (cover, cue, log…). */
  kind: 'track' | 'extra'
  /** The album the file belongs to (the requested one is named as in the catalog). */
  album: string
  /** Checked by default (the requested album). */
  checked: boolean
  /** A track's disc and number from its file name (absent: unknown). */
  disc?: number
  no?: number
  /** The track is the searched song (ReleaseFilesRequest.song). */
  matched?: boolean
  /** In the download «Загрузки» has of this release (ReleaseFiles.inDownload). */
  downloading?: boolean
  /** The library's album has this song already. */
  inLibrary?: boolean
}

export interface ReleaseFiles {
  infoHash: string
  name: string
  /** The release is already on the server whole (or without its file list yet): no list, `message` says so. */
  added: boolean
  /** «Загрузки» has this release with some songs (`downloading`): the picked ones join that download. */
  inDownload?: boolean
  message?: string
  files: ReleaseFile[]
}

/** POST x/acquire. */
export interface AcquireRequest {
  tmdbId: number
  kind: string
  /** Music and books: the catalog id, the album or book, its artist or author and year (the album's folder). */
  extId?: string
  title?: string
  artist?: string
  year?: number
  /** Music: other names of the album (Candidate.aliases). */
  aliases?: string[]
  season?: number
  episode?: number
  mode: 'stream' | 'download'
  releaseLink?: string
  /** Music: the name of the chosen release (evidence for the organize check). */
  releaseTitle?: string
  sourceId?: number
  force?: boolean
  /** releaseLink is a file of an OPDS catalog (Release.direct): downloaded as is. */
  direct?: boolean
  /** Music, a chosen release: the file indexes to download (the track step); absent = the whole release. */
  files?: number[]
  /** Another release chosen for the title: this download replaces the title's unfinished ones (movies and series). */
  replace?: boolean
  /** Music, the server's pick: «Скачать в другом формате» — a release of another format than «Формат загрузки» will do. */
  anyFormat?: boolean
  /** Books: the Open Library cover the candidate showed (the server caches it; any other host is ignored). */
  poster?: string
  /** Movies and series, the server's pick: the quality asked for ('' | 720p | 1080p | 2160p); none of it found, the best other (AcquireResult.note says so). */
  quality?: string
}

export interface AcquireResult {
  infoHash: string
  fileIndex: number
  streamUrl: string
  status: string
  /** Picked songs added to the release's download in «Загрузки» (or the album was downloading already). */
  existing?: boolean
  /** Where the songs go, when it is worth saying (localized). */
  note?: string
}

/** GET x/acquire/progress: a title's download for the download button's ring (null: nothing downloading). */
export interface TitleProgress {
  status: 'active' | 'paused' | 'done'
  done: number
  size: number
  /** active only: every active download of the title is stalled. */
  stalled?: TorrentStalled
  /** active only: see TorrentHint. */
  liveness?: TorrentLiveness
  trackerError?: string
  trackerErrorKind?: TrackerErrorKind
}

/** GET x/torrents/{ih}/status?file=: a «Смотреть сейчас» download, with the buffer where the player reads next. */
export interface StreamStatus {
  status: TorrentStatus
  mode: string
  size: number
  done: number
  downRate: number
  peers: number
  seeds: number
  /** Downloaded of needed bytes where the player reads next (?file=; 0 = unknown). */
  bufferHave?: number
  bufferWant?: number
  stalled?: TorrentStalled
}

// --- «Следить» (moved from the core's api/{types,typesFull}.ts) ---

/** GET x/follow: a followed series (new episodes) or movie (once released). */
export interface Follow {
  id: number
  kind: 'series' | 'movie'
  tmdbId: number
  title: string
  /** Full TMDB poster URL, '' = none. */
  poster: string
  active: boolean
  /** RFC 3339; null = not checked yet. */
  lastChecked: string | null
  lastError: string
  /** Aired episodes (a released movie: 1) not in the library and not downloading. */
  missing: number
  /** Profile that added it ('' = unknown or deleted). */
  addedBy: string
  /** The signed-in profile added it (by id, never by name). */
  mine: boolean
  /** vault, or sonarr | radarr: taken over by «Переезд» (inactive until «Перейти на Vault»). */
  source?: 'vault' | 'sonarr' | 'radarr' | (string & {})
  /** The quality its downloads prefer. */
  resolution?: FollowResolution
}

/** The quality «Докачивать новые» prefers ('' = the household's release preferences). */
export type FollowResolution = '' | '720p' | '1080p' | '2160p'

/** addon.torrents.follow-checked (every adult): a check finished; the follow says the rest (GET x/follow). */
export interface FollowChecked {
  followId: number
}

/** addon.torrents.admin.follow-queued (admins): what a check queued («S01E08», «S02», «фильм»). */
export interface FollowQueued {
  followId: number
  queued: string[]
}

// --- the household's pinned release of a title (moved from the core's api/typesFull.ts) ---

/** A newer release of a pinned title found elsewhere: offered, applied only by «Заменить». */
export interface PinSuggestion {
  releaseTitle: string
  episodes: number
  found: string
}

/** GET x/pinned/{kind}/{tmdbId}: the household's pinned release of a title. */
export interface Pin {
  /** '' for a magnet or an own .torrent file (never refreshed). */
  sourceId: string
  link: string
  details: string
  releaseTitle: string
  infoHash: string
  /** '1080p', '' = unknown. */
  quality: string
  /** How many episodes the release holds (a film: 0). */
  episodes: number
  pinnedAt: string
  lastChecked: string | null
  /** Why the last refresh kept the pin ('' = fine), in the profile's language. */
  lastError: string
  suggestion: PinSuggestion | null
}

/** A title off the library: its name and poster, kept by the server with the pin. */
export interface PinTitle {
  name?: string
  poster?: string
}

// --- «Найти озвучку…» (copied from the core's api/types.ts with the sheet) ---

/** What a voice-over release covers of the title on the server (GET /api/items/{id}/voiceovers). */
export interface VoiceOverCoverage {
  /** Episodes of the server it has a track for, of `total` (a film: 1 of 1, `film`). */
  have: number
  total: number
  /** The release says which seasons (its name, or its file list: `exact`). */
  known: boolean
  exact: boolean
  film?: boolean
}

/** «Найти озвучку…»: a release with a voice-over of the title (`video`: a video release that may hold separate tracks). */
export interface VoiceOverCandidate {
  /** Names it in the search (POST takes it back). */
  id: string
  sourceId: number
  sourceName: string
  title: string
  studio: string
  studios: string[]
  seeders: number
  size: number
  infoHash?: string
  coverage: VoiceOverCoverage
  video?: boolean
}

export interface VoiceOvers {
  candidates: VoiceOverCandidate[]
  videoReleases: VoiceOverCandidate[]
  failed: { sourceId: number; name: string; message: string }[]
}

/** POST /api/items/{id}/voiceovers/files: a release's file list as the episodes it covers. */
export interface VoiceOverPreview {
  id: string
  coverage: VoiceOverCoverage
  episodes: VoiceOverEpisode[]
  /** Why nothing can be taken from it. */
  message?: string
}

/** A voice-over's episode (season/episode absent: the film) and what became of its track. */
export interface VoiceOverEpisode {
  season?: number
  episode?: number
  fileId: number
  state?: 'downloading' | 'placed' | 'fitted' | 'unmeasured' | 'misfit' | 'skipped' | 'had'
  /** fitted: ms, positive = later on the video's timeline. */
  offsetMs?: number
  /** fitted: the speed correction (atempo; 1 or absent = none). */
  tempo?: number
}

/** GET /api/items/{id}/voiceovers/downloads: a voice-over download of the title. */
export interface VoiceOverDownload {
  infoHash: string
  title: string
  studio: string
  status: string
  /** 0…1 */
  progress: number
  done: boolean
  episodes: VoiceOverEpisode[]
}

// --- «Переезд» (admins; x/admin/migrate…, the core's /api/migrate shapes) ---

/** A report of the core's shape, plus what only this addon's runs say. */
export type MigrateReport = CoreMigrateReport & {
  /** History only: the downloads this run moved. */
  torrents?: number
}
/** GET x/admin/migrate: the old services, their users and what does not move. */
export type MigrateStatus = Awaited<ReturnType<MigrateApi['migrateStatus']>>
/** GET x/admin/migrate/state: what changes during a run (no old database read). */
export type MigrateState = Awaited<ReturnType<MigrateApi['migrateState']>>
/** POST x/admin/migrate/run. */
export type MigrateRunRequest = Parameters<MigrateApi['migrateRun']>[0]

/** GET x/admin/migrate/switch: what «Перейти на …» would turn on and download. */
export interface MigrateSwitchPlan {
  follows: number
  episodes: number
  movies: number
  errors: number
  /** Why nothing can be counted now (full disk, …): the switch waits; '' = none. */
  blocked: string
}
