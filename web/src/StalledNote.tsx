// The quiet line under a download that makes no headway: why («Нет
// участников раздачи — ждём…») and, where a page can offer one, «Выбрать
// другую раздачу…» — the same action as the download button's menu. Nothing
// without a stall hint. The addon's own: the core has no such line to share.
import { ui } from '@antresol/addon-sdk'
import type { TorrentHint, TorrentStalled } from './types'
import { useT } from './i18n'
import { hintLine } from './stalled'
import styles from './StalledNote.module.css'

const { Button, ButtonLink } = ui

export function StalledNote({ stalled, hint, noneAlive, onReplace, replaceTo, className }: {
  stalled: TorrentStalled | undefined
  /** The probe's and the trackers' word (liveness, trackerError…). */
  hint?: TorrentHint
  /** Antresol looked for a live release in its place and found none. */
  noneAlive?: boolean
  /** «Выбрать другую раздачу…» on this page. */
  onReplace?: () => void
  /** «Выбрать другую раздачу…» on another page (the title's). */
  replaceTo?: string
  className?: string
}) {
  const t = useT()
  const line = noneAlive ? t('comp.stalled.noneAlive') : hintLine(t, { ...hint, stalled })
  if (!line) return null
  const label = t('item.download.other')
  return (
    <div className={className ? `${styles.note} ${className}` : styles.note} data-testid="stalled-note">
      <span role="status">{line}</span>
      {onReplace ? (
        <Button variant="link" className={styles.action} onClick={onReplace}>
          {label}
        </Button>
      ) : replaceTo ? (
        <ButtonLink variant="link" className={styles.action} to={replaceTo}>
          {label}
        </ButtonLink>
      ) : null}
    </div>
  )
}
