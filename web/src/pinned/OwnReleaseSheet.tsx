// «Добавить свою раздачу»: a magnet, a topic URL of a connected tracker or
// a .torrent file becomes the title's pin. Typing a link drops a chosen file
// and choosing a file clears the link: one of them is sent. A file goes as
// base64 in JSON; one over 8 MiB is refused before anything is sent.
// Only PinnedRelease opens it.
import { ApiError, ui } from '@antresol/addon-sdk'
import { useRef, useState } from 'react'
import { MAX_OWN_TORRENT, pinOwn } from '../api'
import { useT } from '../i18n'
import type { Pin, PinTitle } from '../types'
import styles from './PinnedRelease.module.css'

const { Button, Sheet, TextField } = ui

export function OwnReleaseSheet({
  kind,
  tmdbId,
  title,
  onClose,
  onPinned,
}: {
  kind: 'series' | 'movie'
  tmdbId: number
  title?: PinTitle
  onClose: () => void
  onPinned: (p: Pin) => void
}) {
  const t = useT()
  const [input, setInput] = useState('')
  const [file, setFile] = useState<File | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const picker = useRef<HTMLInputElement>(null)
  const ready = Boolean(file || input.trim())

  const submit = () => {
    if (!ready || busy) return
    setBusy(true)
    setError('')
    pinOwn(kind, tmdbId, file ? { file } : { input: input.trim() }, title).then(
      (p) => onPinned(p),
      (e: unknown) => {
        setBusy(false)
        setError(e instanceof ApiError ? e.message : t('app.failedTryAgain'))
      },
    )
  }
  return (
    <Sheet
      title={t('pin.ownTitle')}
      message={t('pin.ownMessage')}
      role="dialog"
      busy={busy}
      onCancel={onClose}
      actions={[
        { label: t('common.cancel'), kind: 'cancel', run: onClose },
        { label: t('pin.pin'), kind: 'primary', disabled: !ready, run: submit },
      ]}
    >
      <div className={styles.form}>
        <TextField
          label={t('pin.ownField')}
          value={input}
          placeholder="magnet:?xt=…"
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => {
            setInput(e.target.value)
            setFile(null)
          }}
          onKeyDown={(e) => e.key === 'Enter' && submit()}
        />
        <input
          ref={picker}
          type="file"
          accept=".torrent,application/x-bittorrent"
          className="visually-hidden"
          tabIndex={-1}
          aria-hidden
          data-testid="own-torrent-file"
          onChange={(e) => {
            const f = e.target.files?.[0]
            if (f) {
              setInput('')
              if (f.size > MAX_OWN_TORRENT) {
                // refused here: nothing is read or sent
                setFile(null)
                setError(t('pin.ownTooBig'))
              } else {
                setFile(f)
                setError('')
              }
            }
            e.target.value = ''
          }}
        />
        <Button size="small" variant="secondary" disabled={busy} onClick={() => picker.current?.click()}>
          {t('pin.ownFile')}
        </Button>
        {file && <p className={styles.file}>{t('pin.ownFileChosen', { name: file.name })}</p>}
        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
      </div>
    </Sheet>
  )
}
