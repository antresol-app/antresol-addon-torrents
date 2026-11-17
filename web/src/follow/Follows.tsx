// «Слежение»: the series and movies Antresol downloads by itself — new
// episodes as they air, a movie once it is out. Each card says when it was
// checked and what is missing; «Проверить сейчас» starts a check on the
// server, whose end comes back as addon.torrents.follow-checked (a check may
// take minutes). That event names only the follow: the toast reads the
// follow again (GET x/follow), with what the check queued from
// admin.follow-queued (admins only; the check button is theirs).
import { ApiError, core, Icon, ui, useEvents, useUser } from '@antresol/addon-sdk'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import { checkFollow, getFollows, setFollowActive, unfollow } from '../api'
import { ago } from '../format'
import { tr as t, useT } from '../i18n'
import m from '../pages/Manage.module.css'
import { setTitle } from '../title'
import type { Follow, FollowChecked, FollowQueued } from '../types'
import { FOLLOW_CHECKED, FOLLOW_QUEUED } from './events'
import styles from './Follows.module.css'

const { Badge, Button, Sheet, Toast, Toggle } = ui

type State = { status: 'loading' } | { status: 'error'; message: string } | { status: 'ready'; list: Follow[] }

/** How long a toast stays. */
const TOAST_MS = 4500

/** «Не хватает 3 серий» / «Ещё не скачан» / «Всё уже есть». */
export function missingText(f: Pick<Follow, 'kind' | 'missing' | 'lastChecked'>): string {
  if (!f.lastChecked) return ''
  if (f.missing <= 0) return f.kind === 'movie' ? '' : t('follows.allAired')
  return f.kind === 'movie' ? t('follows.movieMissing') : t('follows.missing', { count: f.missing })
}

/** The toast of a finished check: what it queued (admins hear it), else the follow's state. */
export function checkedText(e: { queued: string[]; missing: number; lastError: string }): string {
  if (e.queued.length > 0) return t('follows.queued', { list: e.queued.join(', ') })
  if (e.lastError) return e.lastError
  if (e.missing > 0) return t('follows.noTorrents')
  return t('follows.haveAll')
}

/** Long enough for any check (tracker searches are spaced 10 s apart, ≤ 10 per check). */
const CHECK_GIVE_UP_MS = 3 * 60_000

function FollowCard({
  f,
  checking,
  onCheck,
  onChanged,
  onRemoved,
  readOnly,
}: {
  f: Follow
  checking: boolean
  onCheck: () => void
  onChanged: (f: Follow) => void
  onRemoved: () => void
  /** «Мои слежения» (adults): only «Не следить». */
  readOnly?: boolean
}) {
  const t = useT()
  const [busy, setBusy] = useState<'' | 'active' | 'remove'>('')
  const [problem, setProblem] = useState('')
  const [confirm, setConfirm] = useState(false)
  const poster = core.imageUrl(f.poster, 240)
  const missing = missingText(f)

  const toggle = async (on: boolean) => {
    setBusy('active')
    setProblem('')
    onChanged({ ...f, active: on }) // the switch moves at once
    try {
      onChanged(await setFollowActive(f.id, on))
    } catch (e) {
      onChanged({ ...f, active: !on })
      setProblem(e instanceof ApiError ? e.message : t('downloads.failedRetry'))
    } finally {
      setBusy('')
    }
  }
  const remove = async () => {
    setBusy('remove')
    setProblem('')
    try {
      await unfollow(f.id)
      setConfirm(false)
      onRemoved()
    } catch (e) {
      setConfirm(false)
      setProblem(e instanceof ApiError ? e.message : t('downloads.failedRetry'))
    } finally {
      setBusy('')
    }
  }

  return (
    <li className={`${styles.card} ${f.active ? '' : styles.inactive}`} data-testid="follow-card">
      <Link to={`/title/${f.kind}/${f.tmdbId}`} className={styles.poster} tabIndex={-1} aria-hidden>
        {poster ? <img src={poster} alt="" loading="lazy" /> : <Icon name={f.kind === 'series' ? 'tv' : 'film'} size={26} />}
      </Link>
      <div className={styles.body}>
        <Link to={`/title/${f.kind}/${f.tmdbId}`} className={styles.name}>
          {f.title}
        </Link>
        <p className={styles.meta}>
          <Badge>{f.kind === 'series' ? t('common.kind.series') : t('common.kind.movie')}</Badge>
          <span>{f.lastChecked ? t('follows.checkedAgo', { ago: ago(f.lastChecked) }) : t('follows.notChecked')}</span>
        </p>
        {missing && <p className={f.missing > 0 ? styles.missing : styles.complete}>{missing}</p>}
        {/* mostly states, not failures («Не нашлось раздач — попробуем снова…») */}
        {f.lastError && <p className={styles.lastError}>{f.lastError}</p>}
        {f.addedBy && !readOnly && <p className={styles.detail}>{t('downloads.addedBy', { name: f.addedBy })}</p>}
        {problem && (
          <p className={styles.error} role="alert">
            {problem}
          </p>
        )}
      </div>
      {!readOnly && (
        <div className={styles.activeSwitch}>
          <span aria-hidden>{t('follows.active')}</span>
          <Toggle label={t('follows.activeAria', { title: f.title })} checked={f.active} disabled={busy === 'active'} onChange={(on) => void toggle(on)} />
        </div>
      )}
      <div className={styles.actions}>
        {readOnly ? (
          !f.active && <span className={styles.detail}>{t('follows.off')}</span>
        ) : (
          <Button
            size="small"
            variant="secondary"
            icon="restart"
            aria-label={t('follows.checkAria', { title: f.title })}
            loading={checking}
            disabled={!f.active || checking || !!busy}
            onClick={onCheck}
          >
            {t('follows.checkNow')}
          </Button>
        )}
        <Button
          size="small"
          variant="secondary"
          icon="eyeSlash"
          className={styles.remove}
          aria-label={t('follows.unfollowAria', { title: f.title })}
          title={t('follows.unfollow')}
          disabled={!!busy}
          onClick={() => setConfirm(true)}
        >
          <span className={styles.removeLabel}>{t('follows.unfollow')}</span>
        </Button>
      </div>
      {confirm && (
        <Sheet
          title={t('follows.unfollowConfirm', { title: f.title })}
          message={
            f.kind === 'series'
              ? t('follows.unfollowSeries')
              : t('follows.unfollowMovie')
          }
          icon={<Icon name="eyeSlash" size={34} strokeWidth={1.6} />}
          busy={busy === 'remove'}
          onCancel={() => setConfirm(false)}
          actions={[
            { label: t('common.cancel'), kind: 'cancel', run: () => setConfirm(false) },
            { label: t('follows.unfollow'), kind: 'destructive', run: () => void remove() },
          ]}
        />
      )}
    </li>
  )
}

export interface FollowsProps {
  /**
   * «Мои слежения» for adult profiles: only the follows this profile added,
   * each can be unfollowed; no switch, no checks (the admin's «Слежение»
   * has all of them).
   */
  mine?: boolean
}

export function Follows({ mine = false }: FollowsProps) {
  const t = useT()
  const user = useUser()
  const [state, setState] = useState<State>({ status: 'loading' })
  // follows waiting for their follow-checked: id → when the check started
  const [checking, setChecking] = useState<Map<number, number>>(new Map())
  // what a running check queued (admin.follow-queued comes before its follow-checked)
  const queued = useRef<Map<number, string[]>>(new Map())
  const [toast, setToast] = useState<{ text: string; error?: boolean } | null>(null)
  const toastTimer = useRef<number | undefined>(undefined)

  const show = useCallback((text: string, error = false) => {
    setToast({ text, error })
    window.clearTimeout(toastTimer.current)
    toastTimer.current = window.setTimeout(() => setToast(null), TOAST_MS)
  }, [])
  useEffect(() => () => window.clearTimeout(toastTimer.current), [])

  /** The list again; a finished check of `done` (follow-checked) says how it went. */
  const load = useCallback((quiet = false, done?: number) => {
    if (!quiet) setState({ status: 'loading' })
    getFollows().then(
      (list) => {
        setState({ status: 'ready', list })
        if (done !== undefined) {
          const f = list.find((x) => x.id === done)
          const q = queued.current.get(done) ?? []
          queued.current.delete(done)
          if (f) show(checkedText({ queued: q, missing: f.missing, lastError: f.lastError }), Boolean(f.lastError) && q.length === 0)
        }
        // a lost follow-checked (reconnect, sleeping tab): the list tells a finished check
        setChecking((m) => {
          if (m.size === 0) return m
          const n = new Map(m)
          for (const [id, started] of m) {
            const f = list.find((x) => x.id === id)
            const done = !f || (f.lastChecked && Date.parse(f.lastChecked) >= started - 1000) || Date.now() - started > CHECK_GIVE_UP_MS
            if (done) n.delete(id)
          }
          return n.size === m.size ? m : n
        })
      },
      (e: unknown) => !quiet && setState({ status: 'error', message: e instanceof ApiError ? e.message : t('common.loadFailed') }),
    )
  }, [show])
  useEffect(() => {
    setTitle(mine ? t('follows.mine') : t('follows.manage'))
    load()
  }, [load, mine, t])
  // while a check runs: look again now and then, and when the tab comes back
  const waiting = checking.size > 0
  useEffect(() => {
    if (!waiting) return
    const id = window.setInterval(() => load(true), 30_000)
    const onVisible = () => document.visibilityState === 'visible' && load(true)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.clearInterval(id)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [waiting, load])

  useEvents(
    (e) => {
      if (e.type === FOLLOW_QUEUED) {
        const d = e.data as FollowQueued | null
        if (d && typeof d.followId === 'number' && Array.isArray(d.queued)) queued.current.set(d.followId, d.queued)
        return
      }
      const d = e.data as FollowChecked | null
      const id = d && typeof d.followId === 'number' ? d.followId : undefined
      if (id !== undefined && checking.has(id)) {
        setChecking((s) => {
          const n = new Map(s)
          n.delete(id)
          return n
        })
        return load(true, id)
      }
      load(true)
    },
    user?.isAdmin ? [FOLLOW_CHECKED, FOLLOW_QUEUED] : [FOLLOW_CHECKED],
  )

  const check = async (f: Follow) => {
    setChecking((s) => new Map(s).set(f.id, Date.now()))
    try {
      await checkFollow(f.id)
    } catch (e) {
      setChecking((s) => {
        const n = new Map(s)
        n.delete(f.id)
        return n
      })
      show(e instanceof ApiError ? e.message : t('follows.checkFailed'), true)
    }
  }
  const replace = (f: Follow) => setState((s) => (s.status === 'ready' ? { ...s, list: s.list.map((x) => (x.id === f.id ? f : x)) } : s))
  const drop = (id: number) => setState((s) => (s.status === 'ready' ? { ...s, list: s.list.filter((x) => x.id !== id) } : s))

  const all = state.status === 'ready' ? state.list : []
  // the server lists only this profile's follows for adults; the filter
  // keeps «Мои слежения» right for the admin too
  const list = mine ? all.filter((f) => f.mine) : all
  const series = list.filter((f) => f.kind === 'series').length
  return (
    <section aria-labelledby="follows-title">
      <header className={m.header}>
        <div>
          <h2 id="follows-title" className={m.title}>
            {mine ? t('follows.mine') : t('follows.manage')}
          </h2>
          <p className={m.subtitle}>
            {list.length > 0
              ? [series > 0 ? t('follows.seriesCount', { count: series }) : '', list.length - series > 0 ? t('follows.moviesCount', { count: list.length - series }) : '']
                  .filter(Boolean)
                  .join(' · ')
              : t('follows.subtitle')}
          </p>
        </div>
      </header>
      {state.status === 'loading' && (
        <div className={m.spinnerRow} role="status">
          <span className={m.spinner} aria-hidden /> {t('downloads.loading')}
        </div>
      )}
      {state.status === 'error' && (
        <div className={m.problem} role="alert">
          <Icon name="cloud" size={18} /> {state.message}
          <Button size="small" variant="secondary" onClick={() => load()}>
            {t('common.retry')}
          </Button>
        </div>
      )}
      {state.status === 'ready' && list.length === 0 && (
        <div className={m.empty}>
          <Icon name="eye" size={40} strokeWidth={1.4} />
          <h3>{t('follows.empty')}</h3>
          <p>{t('follows.emptyHint')}</p>
        </div>
      )}
      {list.length > 0 && (
        <>
          <ul className={styles.list}>
            {list.map((f) => (
              <FollowCard
                key={f.id}
                f={f}
                checking={checking.has(f.id)}
                onCheck={() => void check(f)}
                onChanged={replace}
                onRemoved={() => drop(f.id)}
                readOnly={mine}
              />
            ))}
          </ul>
          <p className={m.groupFoot}>
            {mine
              ? t('follows.footMine')
              : t('follows.footAll')}
          </p>
        </>
      )}
      {/* the design system's toast: its live region is always mounted */}
      <Toast messageKey={toast?.text} tone={toast?.error ? 'error' : 'default'}>
        {toast?.text}
      </Toast>
    </section>
  )
}
