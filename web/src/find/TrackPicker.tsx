// The track step of an album release: the release's files, grouped per
// album, each song (number, title, size) with a checkbox, the album's other
// files (cover, cue, log) behind one «Обложка и прочее (N)» checkbox.
// «Выбрать треки…» (preset 'searched'): the song the person searched for
// (the server marks it `matched`) is the only one checked, and scrolled
// to; otherwise none is (the album's checkbox takes them all). «На сервер»
// (preset 'album'): the server's default, the requested album; a release
// of that album alone goes whole at once (onWhole), without the step.
// Songs the release's download in «Загрузки» has, or the library's album,
// are shown checked-off and disabled: «Уже скачивается», «Уже в медиатеке». While the server fetches the torrent's metadata (up to
// 40 s for a magnet) it says «Получаю список файлов…» and counts the
// seconds.
import { useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, formatSize, ui } from '@antresol/addon-sdk'
import { getReleaseFiles } from '../api'
import type { ReleaseFile, ReleaseFiles, ReleaseFilesRequest } from '../types'
import { tr, useT } from '../i18n'
import styles from './TrackPicker.module.css'

const { Button } = ui

export interface TrackPickerProps {
  request: ReleaseFilesRequest
  onBack: () => void
  /** The chosen file indexes (songs plus the extras of the albums they belong to), sorted. */
  onConfirm: (files: number[]) => void
  /** The acquire is running. */
  busy: boolean
  /** The acquire failed: shown in the footer, next to «На сервер», not above the scrolled list. */
  error?: string
  /** What is checked first: the searched song (default) or the server's choice (the requested album). */
  preset?: 'searched' | 'album'
  /** preset 'album': the release is that album alone (nothing to choose) — download it whole instead. */
  onWhole?: () => void
}

/** A file that cannot be picked: the download or the library has it. */
const taken = (f: ReleaseFile) => !!f.downloading || !!f.inLibrary

/** Nothing to choose: one album, all of it the server's default, none of it on the server yet. */
export const wholeByDefault = (d: ReleaseFiles) => !d.inDownload && d.files.length > 0 && d.files.every((f) => f.checked && !taken(f))

type State = { status: 'loading' } | { status: 'error'; message: string } | { status: 'ready'; data: ReleaseFiles }

interface Group {
  album: string
  tracks: ReleaseFile[]
  extras: ReleaseFile[]
}

/** Files per album, in the server's order (sorted by path). */
function groupsOf(files: ReleaseFile[]): Group[] {
  const out: Group[] = []
  const byAlbum = new Map<string, Group>()
  for (const f of files) {
    let g = byAlbum.get(f.album)
    if (!g) {
      g = { album: f.album, tracks: [], extras: [] }
      byAlbum.set(f.album, g)
      out.push(g)
    }
    ;(f.kind === 'track' ? g.tracks : g.extras).push(f)
  }
  return out
}

const baseName = (p: string) => p.slice(p.lastIndexOf('/') + 1)
/** A song's name from its file: «01. Ain't My Bitch.mp3» → «Ain't My Bitch» (the number and extension mean nothing to a listener). */
export const songName = (p: string) => {
  const stem = baseName(p).replace(/\.[a-z0-9]{2,4}$/i, '')
  // a number set off by a separator («01. », «07 - », «3) »), a disc-track
  // pair («1-03 ») or a zero-padded one («01 Song»): a bare «99 Luftballons»
  // or «21 Guns» is the title itself (as the server's cleanTagTitle keeps it)
  return stem.replace(/^(?:\d{1,2}-\d{1,3}(?:\s*[-.)_]\s*|\s+)|\d{1,3}\s*[-.)_]\s*|0\d{1,2}\s+)(?=\S)/, '') || stem
}

/** A track's number as the release names it: «3», «1-03» on a disc; '' when unknown. */
export const trackNo = (f: ReleaseFile) => (f.no ? (f.disc ? `${f.disc}-${String(f.no).padStart(2, '0')}` : String(f.no)) : '')

/** Seconds since `on` turned true (0 while false), ticking once a second. */
function useElapsed(on: boolean): number {
  const [n, setN] = useState(0)
  useEffect(() => {
    setN(0)
    if (!on) return
    const start = Date.now()
    const id = window.setInterval(() => setN(Math.floor((Date.now() - start) / 1000)), 1000)
    return () => window.clearInterval(id)
  }, [on])
  return n
}

export function TrackPicker({ request, onBack, onConfirm, busy, error, preset = 'searched', onWhole }: TrackPickerProps) {
  const t = useT()
  const [state, setState] = useState<State>({ status: 'loading' })
  const waited = useElapsed(state.status === 'loading')
  const [attempt, setAttempt] = useState(0)
  const [tracks, setTracks] = useState<Set<number>>(new Set())
  /** Albums whose «Обложка и прочее» is checked. */
  const [extras, setExtras] = useState<Set<string>>(new Set())
  const root = useRef<HTMLDivElement>(null)
  const confirmButton = useRef<HTMLButtonElement>(null)
  // the latest onWhole, so a new callback per render does not fetch again
  const whole = useRef(onWhole)
  whole.current = onWhole
  const [goingWhole, setGoingWhole] = useState(false)

  useEffect(() => {
    const ac = new AbortController()
    setState({ status: 'loading' })
    setGoingWhole(false)
    getReleaseFiles(request).then(
      (data) => {
        // a late answer of an earlier request (the post takes no signal)
        if (ac.signal.aborted) return
        if (preset === 'album' && whole.current && wholeByDefault(data)) {
          setGoingWhole(true)
          whole.current()
          return
        }
        // «Выбрать треки…»: only the searched song, else none; «На сервер»: the requested album
        const first = (f: ReleaseFile) => f.kind === 'track' && !taken(f) && (preset === 'album' ? f.checked : !!f.matched)
        setTracks(new Set(data.files.filter(first).map((f) => f.index)))
        setExtras(new Set(data.files.filter((f) => f.kind === 'extra' && f.checked).map((f) => f.album)))
        setState({ status: 'ready', data })
      },
      (e: unknown) => {
        if (!ac.signal.aborted) setState({ status: 'error', message: e instanceof ApiError ? e.message : tr('comp.tracks.failed') })
      },
    )
    return () => ac.abort()
  }, [request, attempt, preset])

  const groups = useMemo(() => (state.status === 'ready' ? groupsOf(state.data.files) : []), [state])

  // Each step replaces what had focus (the release's button, the spinner):
  // keep focus inside the sheet — on the first checkbox or button, else the step itself.
  useEffect(() => {
    const el = root.current
    if (!el) return
    const matched = el.querySelector<HTMLElement>('[data-matched] input')
    const first = matched ?? el.querySelector<HTMLElement>('input:not(:disabled), button:not(:disabled)')
    ;(first ?? el).focus()
    // the searched song may be far down a discography
    matched?.scrollIntoView?.({ block: 'center' })
  }, [state.status])

  // «На сервер» is disabled while the acquire runs, which drops its focus out
  // of the sheet (and out of its Tab trap): hold it on the step meanwhile,
  // then give it back to «На сервер» (a failure is shown next to it).
  const wasBusy = useRef(busy)
  useEffect(() => {
    if (wasBusy.current === busy) return
    wasBusy.current = busy
    const el = root.current
    if (!el || el.contains(document.activeElement)) return
    if (busy) el.focus()
    else (confirmButton.current && !confirmButton.current.disabled ? confirmButton.current : el).focus()
  }, [busy])

  return (
    <div ref={root} tabIndex={-1} className={styles.root}>
      {body()}
    </div>
  )

  function body() {

  if (goingWhole) {
    if (error && !busy) {
      return (
        <div className={styles.state}>
          <p role="alert">{error}</p>
          <Button size="small" variant="secondary" onClick={onBack}>
            {t('common.back')}
          </Button>
        </div>
      )
    }
    return (
      <div className={styles.state} role="status">
        <span className={styles.spinner} aria-hidden />
        {t('comp.tracks.whole')}
      </div>
    )
  }
  if (state.status === 'loading') {
    return (
      <div className={styles.state} role="status">
        <span className={styles.spinner} aria-hidden />
        {t('comp.tracks.loading')}
        {/* not announced each second: the status says what happens */}
        <span className={styles.waited} aria-hidden>
          {waited > 0 ? t('comp.tracks.waited', { seconds: waited }) : t('comp.tracks.mayTake')}
        </span>
      </div>
    )
  }
  if (state.status === 'error') {
    return (
      <div className={styles.state}>
        <p role="alert">{state.message}</p>
        <div className={styles.stateButtons}>
          <Button size="small" variant="plain" onClick={onBack}>
            {t('common.back')}
          </Button>
          <Button size="small" variant="secondary" onClick={() => setAttempt((n) => n + 1)}>
            {t('common.retry')}
          </Button>
        </div>
      </div>
    )
  }
  if (state.data.added) {
    return (
      <div className={styles.state}>
        {state.data.message && <p role="status">{state.data.message}</p>}
        <Button size="small" variant="secondary" onClick={onBack}>
          {t('common.back')}
        </Button>
      </div>
    )
  }

  const total = groups.reduce((n, g) => n + g.tracks.filter((f) => !taken(f)).length, 0)
  const chosen = tracks.size

  const toggleTrack = (i: number, on: boolean) =>
    setTracks((s) => {
      const n = new Set(s)
      if (on) n.add(i)
      else n.delete(i)
      return n
    })
  const toggleExtras = (album: string, on: boolean) =>
    setExtras((s) => {
      const n = new Set(s)
      if (on) n.add(album)
      else n.delete(album)
      return n
    })
  const toggleAlbum = (g: Group, on: boolean) => {
    setTracks((s) => {
      const n = new Set(s)
      for (const t of g.tracks) {
        if (taken(t)) continue
        if (on) n.add(t.index)
        else n.delete(t.index)
      }
      return n
    })
    if (g.extras.length) toggleExtras(g.album, on)
  }

  // the extras of an album without a chosen song stay behind (their box shows it: unchecked, disabled)
  const kept = (g: Group) => g.tracks.length === 0 || g.tracks.some((t) => tracks.has(t.index))
  const free = (g: Group) => g.tracks.filter((t) => !taken(t))
  const confirm = () => {
    const out = [...tracks]
    for (const g of groups) {
      if (kept(g) && extras.has(g.album)) out.push(...g.extras.map((f) => f.index))
    }
    onConfirm(out.sort((a, b) => a - b))
  }

  return (
    <div className={styles.picker}>
      <p className={styles.release} title={state.data.name}>
        {state.data.name}
      </p>
      {groups.map((g) => (
        <section key={g.album} className={styles.group}>
          {free(g).length > 0 ? (
            <AlbumCheckbox group={{ ...g, tracks: free(g) }} tracks={tracks} extrasOn={extras.has(g.album)} onToggle={(on) => toggleAlbum(g, on)} />
          ) : (
            <h3 className={styles.album}>{g.album}</h3>
          )}
          <ul className={styles.list}>
            {g.tracks.map((f) => (
              <li key={f.index} data-matched={f.matched || undefined}>
                <label className={`${styles.row} ${styles.trackRow} ${f.matched ? styles.matched : ''}`}>
                  <input
                    type="checkbox"
                    className={styles.checkbox}
                    aria-label={songName(f.path)}
                    aria-describedby={f.matched || taken(f) ? `note-${f.index}` : undefined}
                    checked={tracks.has(f.index)}
                    disabled={taken(f)}
                    onChange={(e) => toggleTrack(f.index, e.target.checked)}
                  />
                  <span className={styles.no} aria-hidden>
                    {trackNo(f)}
                  </span>
                  <span className={styles.trackName}>
                    <span className={styles.name} title={f.path}>
                      {songName(f.path)}
                    </span>
                    {(f.matched || taken(f)) && (
                      <span id={`note-${f.index}`} className={taken(f) ? styles.taken : styles.searched}>
                        {f.downloading ? t('comp.tracks.downloading') : f.inLibrary ? t('comp.tracks.inLibrary') : t('comp.tracks.searched')}
                      </span>
                    )}
                  </span>
                  <span className={styles.size}>{formatSize(f.size)}</span>
                </label>
              </li>
            ))}
            {g.extras.length > 0 && (
              <li>
                <label className={styles.row}>
                  <input
                    type="checkbox"
                    className={styles.checkbox}
                    aria-label={t('comp.tracks.extras', { count: g.extras.length })}
                    checked={kept(g) && extras.has(g.album)}
                    disabled={!kept(g)}
                    onChange={(e) => toggleExtras(g.album, e.target.checked)}
                  />
                  <span className={styles.extras}>
                    <span className={styles.name}>{t('comp.tracks.extras', { count: g.extras.length })}</span>
                    <span className={styles.extraNames}>{g.extras.map((f) => baseName(f.path)).join(', ')}</span>
                  </span>
                  <span className={styles.size}>{formatSize(g.extras.reduce((n, f) => n + f.size, 0))}</span>
                </label>
              </li>
            )}
          </ul>
        </section>
      ))}
      <div className={styles.footer}>
        <div className={styles.count}>
          <span>
            {t('comp.tracks.chosen', { chosen, total })}
          </span>
          {chosen === 0 && <span className={styles.hint}>{t('comp.tracks.pickOne')}</span>}
          {error && !busy && (
            <span className={styles.hint} role="alert">
              {error}
            </span>
          )}
        </div>
        <div className={styles.buttons}>
          <Button size="small" variant="plain" disabled={busy} onClick={onBack}>
            {t('common.back')}
          </Button>
          <Button ref={confirmButton} size="small" variant="primary" icon="download" loading={busy} disabled={busy || chosen === 0} onClick={confirm}>
            {t('comp.tracks.download', { count: chosen })}
          </Button>
        </div>
      </div>
    </div>
  )
  }
}

/** A tri-state checkbox of one album: all its songs at once. */
function AlbumCheckbox({ group, tracks, extrasOn, onToggle }: { group: Group; tracks: Set<number>; extrasOn: boolean; onToggle: (on: boolean) => void }) {
  const ref = useRef<HTMLInputElement>(null)
  const items = group.tracks.length ? group.tracks.map((t) => tracks.has(t.index)) : [extrasOn]
  const all = items.every(Boolean)
  const some = items.some(Boolean)
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = some && !all
  }, [some, all])
  return (
    <label className={`${styles.row} ${styles.albumRow}`}>
      <input ref={ref} type="checkbox" className={styles.checkbox} aria-label={group.album} checked={all} onChange={() => onToggle(!all)} />
      <span className={styles.album}>{group.album}</span>
    </label>
  )
}
