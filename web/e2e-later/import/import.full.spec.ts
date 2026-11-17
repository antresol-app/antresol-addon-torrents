// «Управление → Импорт»: the tiles come from the server, a CSV upload goes to
// the real server (multipart with the upload header) and its preview is the
// real one, the choices are saved there; the run after «Начать импорт» is
// played by page routes (fakes.ts: no MusicBrainz or torrents in e2e):
// matching → downloading with a song to decide → the song resolved → done.
import { IMPORT_CSV, fakeImportRun } from './fakes'
import { fakeDeezerPublic, fakeSpotifyConnect, fakeYandexConnect, fakeYouTubeConnect } from './fakesImport.full'
import { expect, setupAdmin, test } from './vault.full'

test.describe.configure({ timeout: 60_000 })

test('upload, preview, start, progress, a decision, done', async ({ page }) => {
  await setupAdmin(page.request)
  await page.clock.install()
  await page.goto('/manage/import')
  await expect(page.getByRole('heading', { name: 'Импорт медиатеки' })).toBeVisible()

  // the server's tiles: Spotify needs its owner's setup, a file always works
  const tile = (name: string) => page.getByTestId('import-tile').filter({ has: page.getByRole('heading', { name, exact: true }) })
  await expect(page.getByTestId('import-tile')).toHaveCount(6)
  await expect(tile('Spotify')).toContainText('Не настроено на сервере')
  await tile('Spotify').getByRole('button', { name: 'Как настроить' }).click()
  const setup = page.getByRole('dialog', { name: 'Как настроить Spotify' })
  await expect(setup).toContainText('VAULT_SPOTIFY_CLIENT_ID')
  await setup.getByRole('button', { name: 'Закрыть' }).click()

  const chooser = page.waitForEvent('filechooser')
  await tile('Файл').getByRole('button', { name: 'Загрузить файл' }).click()
  const uploaded = page.waitForResponse((r) => r.url().endsWith('/api/import/files'))
  await (await chooser).setFiles({ name: 'library.csv', mimeType: 'text/csv', buffer: Buffer.from(IMPORT_CSV) })
  const res = await uploaded
  expect(res.status(), await res.text()).toBe(201)
  const base = (await res.json()) as Record<string, unknown>

  const preview = page.getByTestId('import-preview')
  await expect(preview).toContainText('Песен: 12')
  await expect(preview).toContainText('Плейлистов: 2')
  await expect(preview).toContainText('Примерно 0,4 ГБ')
  await expect(preview).toContainText('Свободно на сервере:')

  // the choices are the server's: unticking «Вечер» leaves its songs out
  const patched = page.waitForResponse((r) => r.request().method() === 'PATCH')
  await preview.getByRole('checkbox', { name: 'Вечер' }).uncheck()
  expect(((await (await patched).json()) as { counts: { songs: number } }).counts.songs).toBeLessThan(12)
  await expect(preview).not.toContainText('Песен: 12')
  const back = page.waitForResponse((r) => r.request().method() === 'PATCH')
  await preview.getByRole('checkbox', { name: 'Вечер' }).check()
  await back
  await expect(preview).toContainText('Песен: 12')

  await fakeImportRun(page, base)
  await preview.getByRole('button', { name: 'Начать импорт' }).click()
  const progress = page.getByTestId('import-progress')
  await expect(progress).toContainText('Ищем песни в MusicBrainz')
  await expect(progress).toContainText('Готово песен: 2 из 12')

  // the 10 s poll brings the downloads
  await page.clock.fastForward(11_000)
  await expect(progress).toContainText('Готово песен: 6 из 12')
  const group = (album: string) => page.getByTestId('import-group').filter({ hasText: album })
  await expect(group('Группа крови')).toContainText('Скачивается')
  await expect(group('Звезда по имени Солнце')).toContainText('Уже в медиатеке')
  await expect(group('Радио Африка')).toContainText('Ищем раздачу')
  await expect(group('Актриса Весна')).toContainText('не найдено в источниках')
  await group('Актриса Весна').getByRole('button', { name: 'Повторить' }).click()
  await expect(group('Актриса Весна')).toContainText('В очереди')

  await page.getByRole('button', { name: 'Нужно ваше решение: 1' }).click()
  const review = page.getByRole('dialog', { name: 'Нужно ваше решение' })
  const song = review.getByTestId('review-track')
  await expect(song).toContainText('Несколько исполнителей с таким именем')
  await song.getByRole('button', { name: 'Найти вручную' }).click()
  await song.getByRole('searchbox', { name: 'Альбом или песня' }).fill('Кино Кукушка')
  await song.getByRole('button', { name: 'Найти', exact: true }).click()
  await expect(song).toContainText('Кино (Чёрный альбом) — есть «Кукушка»')
  await song.getByRole('button', { name: 'Выбрать', exact: true }).click()
  await expect(review).toContainText('Все песни разобраны.')
  await review.getByRole('button', { name: 'Закрыть' }).click()

  await expect(progress).toContainText('Импорт завершён')
  await expect(progress).toContainText('Готово песен: 11 из 12')
  await progress.getByRole('button', { name: 'Готово' }).click()
  await expect(page.getByTestId('import-tile')).toHaveCount(6)
})

test('«Подключить» Spotify: the sign-in comes back to «Импорт» with the job reading the library', async ({ page }) => {
  await setupAdmin(page.request)
  await page.clock.install()
  await fakeSpotifyConnect(page)
  await page.goto('/manage/import')
  const spotify = page.getByTestId('import-tile').filter({ has: page.getByRole('heading', { name: 'Spotify', exact: true }) })
  await expect(spotify).not.toContainText('Не настроено на сервере')
  const landed = page.waitForRequest((r) => r.url().endsWith('/manage/import?job=1') && r.isNavigationRequest())
  await spotify.getByRole('button', { name: 'Подключить' }).click()
  await landed
  await expect(page.getByText('Читаем медиатеку…')).toBeVisible()
  // the address is cleaned: a reload does not ask for the job again
  await expect(page).toHaveURL(/\/manage\/import$/)

  // the 10 s poll brings the preview; Spotify gave no songs of a followed playlist
  await page.clock.fastForward(11_000)
  const preview = page.getByTestId('import-preview')
  await expect(preview).toContainText('Песен: 3')
  await expect(preview.getByRole('checkbox', { name: /Чужой плейлист/ })).toBeDisabled()
  await expect(preview).toContainText('Spotify не отдаёт песни чужих плейлистов.')
})

test('«Подключить» Яндекс Музыку: the code to enter, the poll, then the job reading the library', async ({ page }) => {
  await setupAdmin(page.request)
  await page.clock.install()
  const fake = await fakeYandexConnect(page)
  await page.goto('/manage/import')
  const yandex = page.getByTestId('import-tile').filter({ has: page.getByRole('heading', { name: 'Яндекс Музыка', exact: true }) })
  await expect(yandex).not.toContainText('Не настроено на сервере')
  await expect(yandex.getByRole('button', { name: 'Вставить токен' })).toBeVisible()
  await yandex.getByRole('button', { name: 'Подключить' }).click()
  const sheet = page.getByRole('dialog', { name: 'Яндекс Музыка' })
  await expect(sheet).toContainText('Откройте https://ya.ru/device и введите код ABCD1234')
  await expect(sheet.getByRole('link', { name: 'https://ya.ru/device' })).toHaveAttribute('href', 'https://ya.ru/device')

  // every 5 s: still waiting, then signed in
  await page.clock.fastForward(5_000)
  await expect.poll(fake.polls).toBe(1)
  await expect(sheet).toBeVisible()
  await page.clock.fastForward(5_000)
  await expect(page.getByText('Читаем медиатеку…')).toBeVisible()
  await expect(sheet).toBeHidden()

  // the 10 s poll of the job brings the preview
  await page.clock.fastForward(11_000)
  await expect(page.getByTestId('import-preview')).toContainText('Песен: 2')
})

test('«Подключить» YouTube Music: the sign-in in a new tab, the pasted address, then the job reading the library', async ({ page }) => {
  await setupAdmin(page.request)
  await page.clock.install()
  const fake = await fakeYouTubeConnect(page)
  await page.goto('/manage/import')
  const youtube = page.getByTestId('import-tile').filter({ has: page.getByRole('heading', { name: 'YouTube Music', exact: true }) })
  await expect(youtube).not.toContainText('Не настроено на сервере')
  const popup = page.waitForEvent('popup')
  await youtube.getByRole('button', { name: 'Подключить' }).click()
  const tab = await popup
  await expect(tab).toHaveURL(/\/fake-google-sign-in$/)
  await tab.close()

  const sheet = page.getByRole('dialog', { name: 'Вставить адрес из браузера' })
  const back = 'http://127.0.0.1:8765/vault-import/youtube?state=g1&code=4/0A&scope=https://www.googleapis.com/auth/youtube.readonly'
  await sheet.getByLabel('Адрес из браузера').fill(back)
  await sheet.getByRole('button', { name: 'Продолжить' }).click()
  await expect(page.getByText('Читаем медиатеку…')).toBeVisible()
  await expect(sheet).toBeHidden()
  expect(fake.pasted()).toBe(back)

  // the 10 s poll of the job brings the preview
  await page.clock.fastForward(11_000)
  await expect(page.getByTestId('import-preview')).toContainText('Песен: 3')
})

test('«Публичный плейлист по ссылке» Deezer: no sign-in, the link, then the job reading the playlist', async ({ page }) => {
  await setupAdmin(page.request)
  await page.clock.install()
  const fake = await fakeDeezerPublic(page)
  await page.goto('/manage/import')
  const deezer = page.getByTestId('import-tile').filter({ has: page.getByRole('heading', { name: 'Deezer', exact: true }) })
  // the real server has no Deezer app: the link works without it
  await expect(deezer).toContainText('Не настроено на сервере')
  await deezer.getByRole('button', { name: 'Публичный плейлист по ссылке' }).click()
  const sheet = page.getByRole('dialog', { name: 'Публичный плейлист по ссылке' })
  await sheet.getByLabel('Ссылка на плейлист').fill('https://www.deezer.com/ru/playlist/404404')
  await sheet.getByRole('button', { name: 'Продолжить' }).click()
  await expect(sheet.getByText('Не найдено')).toBeVisible()
  await sheet.getByLabel('Ссылка на плейлист').fill('https://link.deezer.com/s/30aBc')
  await sheet.getByRole('button', { name: 'Продолжить' }).click()
  await expect(page.getByText('Читаем медиатеку…')).toBeVisible()
  await expect(sheet).toBeHidden()
  expect(fake.sent()).toBe('https://link.deezer.com/s/30aBc')

  // the 10 s poll of the job brings the preview
  await page.clock.fastForward(11_000)
  await expect(page.getByTestId('import-preview')).toContainText('Песен: 3')
  await expect(page.getByTestId('import-preview')).toContainText('Дорога')
})
