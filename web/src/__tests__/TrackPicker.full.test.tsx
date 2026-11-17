import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { ReleaseFile, ReleaseFiles, ReleaseFilesRequest } from '../types'
import { songName, TrackPicker, trackNo } from '../find/TrackPicker'

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } })

const REQ: ReleaseFilesRequest = { sourceId: 3, releaseLink: 'https://t/2.torrent', title: 'Группа крови', artist: 'Кино', year: 1988, aliases: [] }
const ALBUM = 'Группа крови (1988)'
const OTHER = 'Звезда по имени Солнце (1989)'
const MB = 1 << 20

const track = (index: number, name: string, album = ALBUM, checked = true, more: Partial<ReleaseFile> = {}): ReleaseFile => ({
  index,
  path: `Кино/${album}/${name}`,
  size: 30 * MB,
  kind: 'track',
  album,
  checked,
  ...more,
})
const extra = (index: number, name: string, album = ALBUM, checked = true): ReleaseFile => ({ index, path: `Кино/${album}/${name}`, size: 200_000, kind: 'extra', album, checked })

const ONE_ALBUM: ReleaseFiles = {
  infoHash: 'ab'.repeat(20),
  name: 'Кино - Группа крови (1988) [FLAC]',
  added: false,
  files: [track(0, '01 Группа крови.flac'), track(1, '02 Закрой за мной дверь.flac'), track(2, '03 Война.flac'), extra(3, 'cover.jpg'), extra(4, 'album.cue'), extra(5, 'rip.log')],
}

const TWO_ALBUMS: ReleaseFiles = {
  infoHash: 'cd'.repeat(20),
  name: 'Кино - Дискография',
  added: false,
  files: [
    track(0, '01 Группа крови.flac'),
    track(1, '02 Закрой за мной дверь.flac'),
    track(2, '03 Война.flac'),
    track(3, '01 Звезда по имени Солнце.flac', OTHER, false),
    track(4, '02 Невесёлая песня.flac', OTHER, false),
    track(5, '03 Место для шага вперёд.flac', OTHER, false),
  ],
}

function stub(...responses: Array<() => Response | Promise<Response>>) {
  let n = 0
  const f = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => {
    const r = responses[Math.min(n, responses.length - 1)]!
    n++
    return r()
  })
  vi.stubGlobal('fetch', f)
  return f
}

function renderPicker(onConfirm = vi.fn(), onBack = vi.fn()) {
  render(<TrackPicker request={REQ} onBack={onBack} onConfirm={onConfirm} busy={false} />)
  return { onConfirm, onBack }
}

const toServer = () => screen.getByRole('button', { name: /^Скачать выбранные \(\d+\)$/ })

/** Checks every song of the album (its tri-state checkbox). */
const checkAlbum = async (album = ALBUM) => fireEvent.click(await screen.findByRole('checkbox', { name: album }))

// the song checkboxes, by their names without numbers and extensions
const SONG = /^(Группа крови|Закрой за мной дверь|Война|Звезда по имени Солнце|Невесёлая песня|Место для шага вперёд)$/

describe('TrackPicker', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('showsWaitingThenFiles', async () => {
    let resolve!: (r: Response) => void
    const f = stub(() => new Promise<Response>((r) => (resolve = r)))
    renderPicker()
    expect(screen.getByRole('status')).toHaveTextContent('Получаю список файлов…')
    expect(screen.getByText('Это может занять до 40 секунд')).toBeInTheDocument()
    const [url, init] = f.mock.calls[0]!
    expect(String(url)).toBe('/api/addons/torrents/x/admin/acquire/files')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toMatchObject({ sourceId: 3, releaseLink: 'https://t/2.torrent', title: 'Группа крови', artist: 'Кино', year: 1988 })
    resolve(json(ONE_ALBUM))
    const tracks = await screen.findAllByRole('checkbox', { name: SONG })
    expect(tracks).toHaveLength(3)
    // nothing searched: no song checked, the person picks
    for (const t of tracks) expect(t).not.toBeChecked()
    expect(screen.getByText('Песен: 0 из 3')).toBeInTheDocument()
    expect(toServer()).toBeDisabled()
    // the album's checkbox takes every song (and the cover along)
    await checkAlbum()
    for (const t of tracks) expect(t).toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'Обложка и прочее (3)' })).toBeChecked()
    expect(screen.getByText('Песен: 3 из 3')).toBeInTheDocument()
    expect(toServer()).toHaveAccessibleName('Скачать выбранные (3)')
  })

  it('countsTheWait', async () => {
    vi.useFakeTimers()
    try {
      stub(() => new Promise<Response>(() => {}))
      renderPicker()
      expect(screen.getByText('Это может занять до 40 секунд')).toBeInTheDocument()
      await act(async () => {
        vi.advanceTimersByTime(3100)
      })
      expect(screen.getByText('Прошло 3 с из 40')).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('searchedSongCheckedAlone', async () => {
    const scrolled = vi.fn()
    Element.prototype.scrollIntoView = scrolled
    try {
      const files: ReleaseFiles = {
        ...TWO_ALBUMS,
        files: TWO_ALBUMS.files.map((f) => (f.index === 4 ? { ...f, matched: true, no: 2 } : { ...f, no: f.index + 1 })),
      }
      stub(() => json(files))
      const { onConfirm } = renderPicker()
      const song = await screen.findByRole('checkbox', { name: 'Невесёлая песня' })
      expect(song).toBeChecked()
      expect(song).toHaveAccessibleDescription('Вы искали')
      for (const box of screen.getAllByRole('checkbox', { name: SONG })) if (box !== song) expect(box).not.toBeChecked()
      expect(screen.getByText('Песен: 1 из 6')).toBeInTheDocument()
      // focused and scrolled to: it may be far down a discography
      await waitFor(() => expect(document.activeElement).toBe(song))
      expect(scrolled).toHaveBeenCalled()
      fireEvent.click(toServer())
      expect(onConfirm).toHaveBeenCalledWith([4])
    } finally {
      delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView
    }
  })

  it('discographyAlbumCheckboxes', async () => {
    stub(() => json(TWO_ALBUMS))
    renderPicker()
    expect(await screen.findByRole('checkbox', { name: 'Группа крови' })).not.toBeChecked()
    await checkAlbum()
    expect(screen.getByRole('checkbox', { name: 'Группа крови' })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'Война' })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'Звезда по имени Солнце' })).not.toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'Место для шага вперёд' })).not.toBeChecked()
    expect(screen.getByText('Песен: 3 из 6')).toBeInTheDocument()
    // album checkboxes: a whole album checked, the other unchecked; a partial album is mixed
    const first = screen.getByRole('checkbox', { name: ALBUM }) as HTMLInputElement
    const second = screen.getByRole('checkbox', { name: OTHER }) as HTMLInputElement
    expect(first).toBeChecked()
    expect(second).not.toBeChecked()
    fireEvent.click(screen.getByRole('checkbox', { name: 'Закрой за мной дверь' }))
    expect(first.indeterminate).toBe(true)
    fireEvent.click(second)
    expect(screen.getByRole('checkbox', { name: 'Невесёлая песня' })).toBeChecked()
    expect(screen.getByText('Песен: 5 из 6')).toBeInTheDocument()
  })

  it('uncheckAndConfirm', async () => {
    stub(() => json(ONE_ALBUM))
    const { onConfirm } = renderPicker()
    await checkAlbum()
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Закрой за мной дверь' }))
    expect(screen.getByText('Песен: 2 из 3')).toBeInTheDocument()
    fireEvent.click(toServer())
    expect(onConfirm).toHaveBeenCalledWith([0, 2, 3, 4, 5])
  })

  it('extrasUncheckedDropsThem', async () => {
    stub(() => json(ONE_ALBUM))
    const { onConfirm } = renderPicker()
    await checkAlbum()
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Обложка и прочее (3)' }))
    fireEvent.click(toServer())
    expect(onConfirm).toHaveBeenCalledWith([0, 1, 2])
  })

  it('confirmDisabledWithNoTracks', async () => {
    stub(() => json(ONE_ALBUM))
    const { onConfirm } = renderPicker()
    await checkAlbum()
    for (const t of await screen.findAllByRole('checkbox', { name: SONG })) fireEvent.click(t)
    expect(screen.getByText('Песен: 0 из 3')).toBeInTheDocument()
    expect(toServer()).toBeDisabled()
    expect(screen.getByText('Выберите хотя бы одну песню')).toBeInTheDocument()
    fireEvent.click(toServer())
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('retryAfterError', async () => {
    const f = stub(
      () => json({ error: 'Не удалось получить список файлов' }, 422),
      () => json(ONE_ALBUM),
    )
    renderPicker()
    expect(await screen.findByRole('alert')).toHaveTextContent('Не удалось получить список файлов')
    fireEvent.click(screen.getByRole('button', { name: 'Повторить' }))
    expect(await screen.findAllByRole('checkbox', { name: SONG })).toHaveLength(3)
    expect(f).toHaveBeenCalledTimes(2)
  })

  it('nonJsonGatewayErrorShowsRetry', async () => {
    stub(() => new Response('<html><body><h1>504 Gateway Time-out</h1></body></html>', { status: 504, headers: { 'Content-Type': 'text/html' } }))
    renderPicker()
    expect(await screen.findByRole('alert')).toHaveTextContent('Не удалось получить список файлов')
    expect(screen.getByRole('button', { name: 'Повторить' })).toBeInTheDocument()
  })

  it('focusStaysInsideOnEachStep', async () => {
    let resolve!: (r: Response) => void
    stub(() => new Promise<Response>((r) => (resolve = r)))
    const { container } = render(<TrackPicker request={REQ} onBack={vi.fn()} onConfirm={vi.fn()} busy={false} />)
    // loading: the step itself holds focus (what had it is gone)
    expect(container.contains(document.activeElement)).toBe(true)
    expect(document.activeElement).not.toBe(document.body)
    resolve(json(ONE_ALBUM))
    const first = await screen.findByRole('checkbox', { name: ALBUM })
    await waitFor(() => expect(document.activeElement).toBe(first))
  })

  it('extrasOfAlbumWithoutSongsStayBehind', async () => {
    const files: ReleaseFiles = {
      ...TWO_ALBUMS,
      files: [...TWO_ALBUMS.files, extra(6, 'cover.jpg'), extra(7, 'cover.jpg', OTHER, true)],
    }
    stub(() => json(files))
    const { onConfirm } = renderPicker()
    await checkAlbum()
    const boxes = await screen.findAllByRole('checkbox', { name: 'Обложка и прочее (1)' })
    // album 2 has its extras box checked by the server but no song: shown unchecked and disabled
    expect(boxes[0]).toBeChecked()
    expect(boxes[1]).not.toBeChecked()
    expect(boxes[1]).toBeDisabled()
    fireEvent.click(toServer())
    expect(onConfirm).toHaveBeenCalledWith([0, 1, 2, 6])
    // a song of album 2 brings its extras along
    fireEvent.click(screen.getByRole('checkbox', { name: 'Невесёлая песня' }))
    expect(boxes[1]).toBeChecked()
    fireEvent.click(toServer())
    expect(onConfirm).toHaveBeenLastCalledWith([0, 1, 2, 4, 6, 7])
  })

  it('albumPresetChecksTheRequestedAlbum', async () => {
    stub(() => json(TWO_ALBUMS))
    const onWhole = vi.fn()
    const onConfirm = vi.fn()
    render(<TrackPicker request={REQ} onBack={vi.fn()} onConfirm={onConfirm} busy={false} preset="album" onWhole={onWhole} />)
    // a discography: the step, the requested album checked, the other not
    expect(await screen.findByRole('checkbox', { name: 'Группа крови' })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'Война' })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: 'Невесёлая песня' })).not.toBeChecked()
    expect(onWhole).not.toHaveBeenCalled()
    fireEvent.click(toServer())
    expect(onConfirm).toHaveBeenCalledWith([0, 1, 2])
  })

  it('albumPresetOfOneAlbumGoesWhole', async () => {
    stub(() => json(ONE_ALBUM))
    const onWhole = vi.fn()
    render(<TrackPicker request={REQ} onBack={vi.fn()} onConfirm={vi.fn()} busy={true} preset="album" onWhole={onWhole} />)
    await waitFor(() => expect(onWhole).toHaveBeenCalledTimes(1))
    expect(screen.getByRole('status')).toHaveTextContent('В раздаче только этот альбом — скачиваю целиком…')
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0)
  })

  it('songsTheDownloadOrLibraryHasAreTaken', async () => {
    const files: ReleaseFiles = {
      ...ONE_ALBUM,
      inDownload: true,
      files: [
        track(0, '01 Группа крови.flac', ALBUM, true, { downloading: true }),
        track(1, '02 Закрой за мной дверь.flac', ALBUM, true, { inLibrary: true, matched: true }),
        track(2, '03 Война.flac'),
        extra(3, 'cover.jpg', ALBUM, true),
      ],
    }
    stub(() => json(files))
    const { onConfirm } = renderPicker()
    const downloading = await screen.findByRole('checkbox', { name: 'Группа крови' })
    expect(downloading).toBeDisabled()
    expect(downloading).not.toBeChecked()
    expect(downloading).toHaveAccessibleDescription('Уже скачивается')
    const inLibrary = screen.getByRole('checkbox', { name: 'Закрой за мной дверь' })
    // searched, but the library's album has it: not checked
    expect(inLibrary).toBeDisabled()
    expect(inLibrary).not.toBeChecked()
    expect(inLibrary).toHaveAccessibleDescription('Уже в медиатеке')
    expect(screen.getByText('Песен: 0 из 1')).toBeInTheDocument()
    // the album's checkbox takes only what can be picked
    await checkAlbum()
    expect(screen.getByRole('checkbox', { name: 'Война' })).toBeChecked()
    expect(inLibrary).not.toBeChecked()
    fireEvent.click(toServer())
    expect(onConfirm).toHaveBeenCalledWith([2, 3])
  })

  it('alreadyAddedShowsMessage', async () => {
    const message = 'Сообщение сервера о том, что раздача уже есть.'
    stub(() => json({ infoHash: 'ab'.repeat(20), name: 'x', added: true, message, files: [] }))
    const { onBack } = renderPicker()
    expect(await screen.findByText(message)).toBeInTheDocument()
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0)
    expect(screen.queryByRole('button', { name: /Скачать выбранные/ })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Назад' }))
    await waitFor(() => expect(onBack).toHaveBeenCalled())
  })
})

describe('trackNo', () => {
  it('shows the number, with its disc when known', () => {
    expect(trackNo(track(0, '03 Война.flac', ALBUM, true, { no: 3 }))).toBe('3')
    expect(trackNo(track(0, '1-03 Война.flac', ALBUM, true, { disc: 1, no: 3 }))).toBe('1-03')
    expect(trackNo(track(0, 'Война.flac'))).toBe('')
  })
})

describe('songName', () => {
  it('drops the track number and the extension', () => {
    expect(songName("Load (1996)/01. Ain't My Bitch.mp3")).toBe("Ain't My Bitch")
    expect(songName('1-03 Cure.flac')).toBe('Cure')
    expect(songName('07 - Bleeding Me.mp3')).toBe('Bleeding Me')
    expect(songName('1979.flac')).toBe('1979')
    expect(songName('01 Song.mp3')).toBe('Song')
    expect(songName('3) Intro.mp3')).toBe('Intro')
    expect(songName('2-11_Outro.ogg')).toBe('Outro')
  })

  it('keeps a number that is part of the title', () => {
    expect(songName('99 Luftballons.mp3')).toBe('99 Luftballons')
    expect(songName('21 Guns.flac')).toBe('21 Guns')
    expect(songName('Album/7 Seconds.mp3')).toBe('7 Seconds')
  })
})
