// The household's pinned release of a title: the release new episodes come
// from — its name, quality, when it was last checked and why a refresh kept
// it; «Обновить сейчас», «Открепить», «Добавить свою раздачу»
// (OwnReleaseSheet) and the offer of a newer version found elsewhere
// («Заменить» / «Не сейчас»). Drawn by the title panels (itemPanels).
import { ApiError, ui } from '@antresol/addon-sdk'
import { useCallback, useEffect, useState } from 'react'
import { acceptPinSuggestion, getPin, refreshPin, unpin } from '../api'
import { ago } from '../format'
import { useT } from '../i18n'
import type { Pin, PinTitle } from '../types'
import { OwnReleaseSheet } from './OwnReleaseSheet'
import styles from './PinnedRelease.module.css'

const { Badge, Button, Card, Notice } = ui

type Kind = 'series' | 'movie'

// A title's pin, shared by what the page shows of it: the panel and the
// release sheets an item action opens (a pin made in one is the other's).
const changed = new Set<(key: string, p: Pin | null) => void>()
const keyOf = (kind: Kind, tmdbId: number) => `${kind}:${tmdbId}`

/**
 * The title's pin, loaded once: `available` is false while loading and
 * when the server has no pins (an error): nothing is shown then. `set`
 * takes the server's answer of a change, here and in every other user of
 * the same title's pin.
 */
export function usePinned(kind: Kind, tmdbId: number, enabled = true) {
  const [pin, setPin] = useState<Pin | null>(null)
  const [available, setAvailable] = useState(false)
  // the answer came (a pin, none, or an error)
  const [settled, setSettled] = useState(false)
  useEffect(() => {
    setPin(null)
    setAvailable(false)
    setSettled(false)
    if (!enabled || tmdbId <= 0) return
    const ac = new AbortController()
    getPin(kind, tmdbId, ac.signal).then(
      (p) => {
        setPin(p)
        setAvailable(true)
        setSettled(true)
      },
      () => !ac.signal.aborted && setSettled(true),
    )
    const mine = keyOf(kind, tmdbId)
    const on = (key: string, p: Pin | null) => key === mine && setPin(p)
    changed.add(on)
    return () => {
      ac.abort()
      changed.delete(on)
    }
  }, [kind, tmdbId, enabled])
  const set = useCallback((p: Pin | null) => {
    for (const on of changed) on(keyOf(kind, tmdbId), p)
  }, [kind, tmdbId])
  return { pin, available, settled, set }
}

const errText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback)

export interface PinnedReleaseProps {
  kind: Kind
  tmdbId: number
  pin: Pin | null
  onChange: (p: Pin | null) => void
  /** A title off the library: its name and poster go with a new pin. */
  title?: PinTitle
}

export function PinnedRelease({ kind, tmdbId, pin, onChange, title }: PinnedReleaseProps) {
  const t = useT()
  const [busy, setBusy] = useState<'refresh' | 'unpin' | 'accept' | null>(null)
  const [error, setError] = useState('')
  const [said, setSaid] = useState('')
  const [owning, setOwning] = useState(false)
  // «Не сейчас»: this offer is not shown again on this page
  const [dismissed, setDismissed] = useState('')

  const act = (what: NonNullable<typeof busy>, run: () => Promise<void>) => {
    setBusy(what)
    setError('')
    setSaid('')
    run()
      .catch((e: unknown) => setError(errText(e, t('app.failedTryAgain'))))
      .finally(() => setBusy(null))
  }
  const refresh = () =>
    act('refresh', async () => {
      const r = await refreshPin(kind, tmdbId)
      onChange(r.pin)
      if (!r.pin.lastError) setSaid(r.changed ? t('pin.refreshed') : t('pin.unchanged'))
    })
  const drop = () =>
    act('unpin', async () => {
      await unpin(kind, tmdbId)
      onChange(null)
    })
  const accept = () => act('accept', async () => onChange(await acceptPinSuggestion(kind, tmdbId)))

  const sg = pin?.suggestion
  const offer = sg && dismissed !== `${sg.releaseTitle}|${sg.found}` ? sg : null
  return (
    <Card as="section" className={styles.card} aria-labelledby={`pin-${kind}-${tmdbId}`}>
      <h2 id={`pin-${kind}-${tmdbId}`} className={styles.heading}>
        {t('pin.title')}
      </h2>
      {pin ? (
        <>
          <p className={styles.name} title={pin.releaseTitle}>
            {pin.releaseTitle}
          </p>
          <p className={styles.meta}>
            {pin.quality && <Badge>{pin.quality}</Badge>}
            {kind === 'series' && pin.episodes > 0 && <Badge>{t('pin.episodes', { count: pin.episodes })}</Badge>}
            {!pin.sourceId && <Badge>{t('pin.own')}</Badge>}
            <span>{pin.lastChecked ? t('pin.checked', { ago: ago(pin.lastChecked) }) : t('pin.notChecked')}</span>
          </p>
          {pin.lastError && <Notice>{pin.lastError}</Notice>}
          {offer && (
            <Notice
              tone="accent"
              title={t('pin.suggestion')}
              actions={
                <>
                  <Button size="small" variant="secondary" loading={busy === 'accept'} disabled={busy !== null} onClick={accept}>
                    {t('pin.replace')}
                  </Button>
                  <Button size="small" variant="secondary" disabled={busy !== null} onClick={() => setDismissed(`${offer.releaseTitle}|${offer.found}`)}>
                    {t('pin.notNow')}
                  </Button>
                </>
              }
            >
              {offer.releaseTitle}
            </Notice>
          )}
        </>
      ) : (
        <p className={styles.none}>{t('pin.none')}</p>
      )}
      <div className={styles.actions}>
        {pin?.sourceId && (
          <Button size="small" variant="secondary" loading={busy === 'refresh'} disabled={busy !== null} onClick={refresh}>
            {t('pin.refresh')}
          </Button>
        )}
        <Button size="small" variant="secondary" disabled={busy !== null} onClick={() => setOwning(true)}>
          {t('pin.addOwn')}
        </Button>
        {pin && (
          <Button size="small" variant="destructive" loading={busy === 'unpin'} disabled={busy !== null} onClick={drop}>
            {t('pin.unpin')}
          </Button>
        )}
      </div>
      {said && (
        <p className={styles.said} role="status">
          {said}
        </p>
      )}
      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}
      {owning && (
        <OwnReleaseSheet
          kind={kind}
          tmdbId={tmdbId}
          title={title}
          onClose={() => setOwning(false)}
          onPinned={(p) => {
            setOwning(false)
            onChange(p)
          }}
        />
      )}
    </Card>
  )
}
