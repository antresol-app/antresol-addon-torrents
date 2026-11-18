// The addon's web part: it registers its pages, «Управление» tiles, its
// slot entries, events and strings when the app loads it. React, the router
// and the SDK are the app's own (the Vite plugin maps the imports); the UI
// is the SDK's `ui`. Downloads, sources and the downloads settings are
// admins'; adults get find-and-add under «Поиск» (owner answer 1). Every
// title and label is a key of `strings`, resolved by the core when it
// draws it, in the interface language.
import { registerAddon, type EmptyStateAction } from '@antresol/addon-sdk'
import { ID } from './api'
import { useActiveDownloads } from './badge/useActiveDownloads'
import { useEpisodePlay } from './episodes/useEpisodePlay'
import { FindAndAddBlock } from './find/FindAndAddBlock'
import { Follows } from './follow/Follows'
import { MyFollows } from './follow/MyFollows'
import { RequestReleaseSheet } from './find/RequestReleaseSheet'
import { AddSource, EditSource } from './pages/AddSource'
import { Downloads } from './pages/Downloads'
import { Settings } from './pages/Settings'
import { Sources } from './pages/Sources'
import { SeedingStats } from './stats/SeedingStats'
import { downloadable, FollowTile, openDownload, TitlePanels, watchableOffLibrary, watchFromTitle } from './titlepage/TitlePanels'
import { flat, type Key } from './strings'
import { KeepAction } from './watch/KeepAction'
import { useWatchProgress } from './watch/useWatchProgress'
import { MigratePageRoute } from './migrate/MigratePageRoute'
import { libraryVideo, openVoiceOverSheet, VoiceOverSheet } from './voices/VoiceOverSheet'
import { WatchNote } from './watch/WatchNote'

// a strings key, checked here; the core resolves it
const k = (key: Key): string => key

/** «Найти и добавить» on an empty library page: «Поиск» with that page's catalog (?dk=); Home's with none. */
const EMPTY_SCOPES: EmptyStateAction['scope'][] = ['home', 'video', 'music', 'book', 'audiobook', 'comic']

registerAddon(ID, {
  routes: [
    { path: 'downloads', admin: true, title: k('common.downloads'), element: () => <Downloads /> },
    { path: 'sources', admin: true, title: k('sources.title'), element: () => <Sources /> },
    { path: 'sources/add', admin: true, title: k('sources.add.title'), element: () => <AddSource /> },
    { path: 'sources/add/:id', admin: true, title: k('sources.add.title'), element: () => <AddSource /> },
    { path: 'sources/:sourceId/edit', admin: true, title: k('sources.title'), element: () => <EditSource /> },
    { path: 'settings', admin: true, title: k('tsettings.title'), element: () => <Settings /> },
    // «Мои слежения» (adults, from the profile menu) and the admins' «Слежение»
    { path: 'follows', title: k('follows.mine'), element: () => <MyFollows /> },
    { path: 'manage/follows', admin: true, title: k('follows.manage'), element: () => <Follows /> },
    // «Переезд» from Prowlarr, Sonarr, Radarr and qBittorrent (the core's page over x/admin/migrate…)
    { path: 'migrate', admin: true, title: k('migrate.addon.title'), element: () => <MigratePageRoute /> },
  ],
  // its card on the core's «Управление → Переезд», after the media servers
  migrateSources: [{ id: 'torrents', name: k('migrate.addon.sources'), icon: 'arrowRight', to: 'migrate', admin: true }],
  manageTiles: [
    { label: k('common.downloads'), icon: 'download', to: 'downloads', admin: true },
    { label: k('manage.section.sources'), icon: 'plug', to: 'sources', admin: true },
    { label: k('tsettings.title'), icon: 'gear', to: 'settings', admin: true },
    { label: k('follows.manage'), icon: 'eye', to: 'manage/follows', admin: true },
  ],
  // «Мои слежения» for adults who are not the admin (the admin has «Управление → Слежение»)
  profileMenu: [{ label: k('follows.mine'), icon: 'eye', to: 'follows', visible: (u) => !u.isAdmin }],
  // «Смотреть сейчас» in the core's input player: the waiting note, «Оставить
  // на сервере» in its top bar, and the download's progress it waits on
  playerNotes: [{ element: WatchNote, actions: KeepAction, useProgress: useWatchProgress }],
  searchBlocks: [{ element: FindAndAddBlock }],
  emptyStateActions: EMPTY_SCOPES.map((scope) => ({
    scope,
    label: k('find.add'),
    icon: 'search' as const,
    to: scope === 'home' ? '/search' : `/search?dk=${scope}`,
  })),
  // the admins' count of running downloads beside the navigation («Загрузки: N»)
  navBadges: [{ to: 'downloads', icon: 'download', label: k('badge.label'), useCount: useActiveDownloads, admin: true }],
  // «Одобрить» in «Просьбы»: the release picker of the requested title
  requestActions: [{ element: RequestReleaseSheet }],
  statsBlocks: [{ element: SeedingStats, admin: true }],
  // «Следить» is a round button of the action row (a tile on a phone); under
  // the row of an item's and a title's page: the pinned release, «Добавить сезон…»
  itemPanels: [{ element: FollowTile, inRow: true }, { element: TitlePanels }],
  // a title off the library: «Смотреть сейчас» among the core's «Смотреть ▾»
  // ways and the round «Скачать»; each run returns its sheet
  itemActions: [
    { label: k('watch.now'), icon: 'play', watchWay: true, visible: watchableOffLibrary, run: watchFromTitle },
    { label: k('download.title'), icon: 'download', visible: downloadable, run: openDownload },
    // a library film or series: the voice-over search of the sources
    { label: k('vo.find'), icon: 'speaker', visible: libraryVideo, run: openVoiceOverSheet },
  ],
  // the player's audio menu: the same search for what plays (never online playback)
  playerTrackActions: [
    {
      label: k('vo.find'),
      icon: 'speaker',
      visible: libraryVideo,
      run: (it, close) => <VoiceOverSheet itemId={it.id} title={it.title} onClose={close} />,
    },
  ],
  // the episode guide's pinned and torrent sources (library and online
  // are the core's): their names, and where they sort on the core's scale
  // (library 0 < pinned < online 20 < torrent)
  episodeActions: [
    {
      kinds: ['pinned', 'torrent'],
      labels: { pinned: k('episodes.kind.pinned'), torrent: k('episodes.kind.torrent') },
      order: { pinned: 10, torrent: 30 },
      use: useEpisodePlay,
    },
  ],
  // short names (contract, Events): `progress` (library downloads, adults),
  // `follow-checked` (adults: a check ended) and admin.progress /
  // admin.source-status / admin.follow-queued, which reach admins only as
  // addon.torrents.admin.<name>; admin.migrate.progress («Переезд») likewise
  events: ['progress', 'source-status', 'follow-checked', 'follow-queued', 'migrate.progress'],
  strings: flat,
})
