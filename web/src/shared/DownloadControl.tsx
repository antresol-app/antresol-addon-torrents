import { DownloadButton, type DownloadState, ui } from '@antresol/addon-sdk'
import type { TorrentHint } from '../types'
import { useState } from 'react'
import { cancelTitleDownload } from '../api'
import { hintLine } from '../stalled'
import type { TitleProgressQuery } from './useTitleProgress'
import styles from './DownloadControl.module.css'
import { useT } from '../i18n'

const { MenuHeading, MenuItem, Menu, Sheet } = ui

/**
 * The round download button of a title that is not on the server. Idle, it
 * opens the release picker (`onPick`); while a download runs or is paused it
 * is a menu: «Выбрать другую раздачу…» (`onReplace`) and «Отменить
 * загрузку», asked first because the downloaded part is deleted.
 */
export function DownloadControl({
  state,
  query,
  size,
  disabled,
  onPick,
  onReplace,
  onCancelled,
}: {
  state: DownloadState<TorrentHint>
  /** The download it cancels (as the ring's progress). */
  query: TitleProgressQuery
  size?: 'large' | 'small'
  disabled?: boolean
  onPick: () => void
  onReplace: () => void
  onCancelled: () => void
}) {
  const t = useT()
  const [asking, setAsking] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (state.kind !== 'active' && state.kind !== 'paused' && !asking) {
    return <DownloadButton state={state} size={size} disabled={disabled} onClick={onPick} />
  }
  const close = () => (setAsking(false), setError(''))
  const cancel = () => {
    setBusy(true)
    setError('')
    cancelTitleDownload(query).then(
      () => {
        setBusy(false)
        setAsking(false)
        onCancelled()
      },
      (e: unknown) => {
        setBusy(false)
        setError(e instanceof Error ? e.message : t('app.failedTryAgain'))
      },
    )
  }
  return (
    <>
      <Menu label={t('item.download.menu')} trigger={(p) => <DownloadButton state={state} size={size} disabled={disabled} triggerProps={p} />}>
        {(closeMenu) => (
          <>
            {state.kind === 'active' && state.stalled && <MenuHeading>{hintLine(t, { ...state.hint, stalled: state.stalled })}</MenuHeading>}
            <MenuItem label={t('item.download.other')} onSelect={() => (closeMenu(false), onReplace())} />
            <MenuItem label={t('item.download.cancel')} destructive onSelect={() => (closeMenu(false), setAsking(true))} />
          </>
        )}
      </Menu>
      {asking && (
        <Sheet
          title={t('item.download.cancelAsk')}
          message={t('item.download.cancelText')}
          busy={busy}
          onCancel={close}
          actions={[
            { label: t('item.download.keep'), kind: 'cancel', run: close },
            { label: t('item.download.cancel'), kind: 'destructive', run: cancel },
          ]}
        >
          {error && (
            <p className={styles.acquireError} role="alert">
              {error}
            </p>
          )}
        </Sheet>
      )}
    </>
  )
}
