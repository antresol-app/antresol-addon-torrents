import { describe, expect, it } from 'vitest'
import { catalogs } from '../strings'

// The rollback confirmation is the admin's only warning before it: it must
// be true with sharing on and off — only the ones that ran in qBittorrent
// before the move start again, and none while «Не раздавать» is on.
describe('migrate.rollback.message', () => {
  it('ru: those that ran start again, paused while no-seeding is on', () => {
    expect(catalogs.ru['migrate.rollback.message']).toEqual({
      one: '{app} перестанет раздавать {count} раздачу и вернёт её в qBittorrent: если она раздавалась там до переноса, она запустится снова, а пока включено «Не раздавать» — останется на паузе. Файлы не удаляются.',
      few: '{app} перестанет раздавать {count} раздачи и вернёт их в qBittorrent: те, что раздавались там до переноса, запустятся снова, а пока включено «Не раздавать» — останутся на паузе. Файлы не удаляются.',
      many: '{app} перестанет раздавать {count} раздач и вернёт их в qBittorrent: те, что раздавались там до переноса, запустятся снова, а пока включено «Не раздавать» — останутся на паузе. Файлы не удаляются.',
    })
  })
  it('en: the same', () => {
    expect(catalogs.en['migrate.rollback.message']).toEqual({
      one: '{app} stops seeding {count} torrent and gives it back to qBittorrent: if it was seeding there before the move, it starts again, but it stays paused while “Don’t upload” is on. Files aren’t deleted.',
      other: '{app} stops seeding {count} torrents and gives them back to qBittorrent: the ones that were seeding there before the move start again, but they stay paused while “Don’t upload” is on. Files aren’t deleted.',
    })
  })
})
