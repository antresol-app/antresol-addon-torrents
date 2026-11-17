// The input player's note while a «Смотреть сейчас» input waits for data
// (playerNotes, in place of «Загружается…»): how much of what the player
// reads next has arrived, and, when the download makes no headway, why
// (StalledNote). A slow swarm is never reported as a device problem.
import { formatRate, type PlayerInput } from '@antresol/addon-sdk'
import { tr as t } from '../i18n'
import { StalledNote } from '../StalledNote'
import type { StreamStatus } from '../types'
import { useWatchStatus, watchOf } from './status'
import styles from './WatchNote.module.css'

/**
 * «Загружается… N%»: N is how much of what the player reads next has
 * arrived (the server's buffer window), not the share of the whole file.
 */
export function bufferingNote(s: StreamStatus | null): string {
  if (!s) return t('player.stream.connecting')
  if (s.status === 'queued' || s.size <= 0) return s.peers > 0 ? t('player.stream.gettingFiles', { peers: t('player.stream.peers', { count: s.peers }) }) : t('player.stream.lookingForPeers')
  const want = s.bufferWant ?? 0
  const pct = want > 0 ? Math.min(99, Math.floor(((s.bufferHave ?? 0) / want) * 100)) : Math.min(99, Math.floor((s.done / s.size) * 100))
  const who = s.peers > 0 ? t('player.stream.peers', { count: s.peers }) : t('player.stream.searchingPeers')
  return t('player.stream.loading', { pct, who }) + (s.downRate > 0 ? ` · ${formatRate(s.downRate)}` : '')
}

export function WatchNote({ input }: { input: PlayerInput }) {
  const status = useWatchStatus(watchOf(input))
  return (
    <span className={styles.note}>
      <span>{bufferingNote(status)}</span>
      {status?.stalled && <StalledNote stalled={status.stalled} className={styles.stalled} />}
    </span>
  )
}
