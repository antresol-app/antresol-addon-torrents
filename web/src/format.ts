// A download's status in words (moved from the core's state/format.ts).
import { locale } from '@antresol/addon-sdk'
import { tr } from './i18n'
import type { TorrentStatus } from './types'

/** «В очереди», «Скачивается», «Пауза»… */
export function torrentStatusLabel(status: TorrentStatus, mode?: string): string {
  switch (status) {
    case 'queued':
      return tr('common.download.queued')
    case 'downloading':
      return mode === 'stream' ? tr('common.download.streaming') : tr('common.download.downloading')
    case 'paused':
      return tr('common.download.paused')
    case 'waiting_space':
      return tr('common.download.waitingSpace')
    case 'error':
      return tr('common.error')
    default:
      return status
  }
}

/** «только что», «5 мин назад», «вчера», a date (moved from the core's state/format.ts with the follows). */
export function ago(iso: string | undefined | null, now = Date.now()): string {
  if (!iso) return ''
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return ''
  const s = Math.max(0, (now - at) / 1000)
  if (s < 60) return tr('common.ago.now')
  if (s < 3600) return tr('common.ago.minutes', { n: Math.floor(s / 60) })
  if (s < 86400) return tr('common.ago.hours', { n: Math.floor(s / 3600) })
  if (s < 2 * 86400) return tr('common.ago.yesterday')
  return new Date(at).toLocaleDateString(locale(), { day: 'numeric', month: 'long', year: new Date(at).getFullYear() === new Date(now).getFullYear() ? undefined : 'numeric' })
}
