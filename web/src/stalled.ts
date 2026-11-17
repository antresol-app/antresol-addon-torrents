// The quiet line under a download that makes no headway (torrent.Status.Stalled):
// the server says why, so a «0 %» that never moves is explained.
import type { TorrentHint, TorrentStalled, TrackerErrorKind } from './types'
import type { TFunction } from './i18n'

/** «Нет участников раздачи — ждём…», «Получение данных о раздаче…»; '' for none (or a hint this app does not know). */
export function stalledLine(t: TFunction, stalled: TorrentStalled | undefined): string {
  switch (stalled) {
    case 'no_peers':
      return t('comp.stalled.noPeers')
    case 'no_metadata':
      return t('comp.stalled.noMetadata')
    default:
      return ''
  }
}

/** A tracker's refusal in plain words; its own (redacted) words when the kind is not known; '' for none. */
export function trackerLine(t: TFunction, kind: TrackerErrorKind | undefined, raw: string | undefined): string {
  switch (kind) {
    case 'bad_passkey':
      return t('comp.tracker.badPasskey')
    case 'not_registered':
      return t('comp.tracker.notRegistered')
    case undefined:
    case '':
      return ''
    default:
      return raw ? t('comp.tracker.said', { text: raw }) : ''
  }
}

/**
 * The line of a download without headway: a tracker's refusal wins over the stall
 * hint; with no stall yet, a probe that saw no seeds says so («Сидов пока не видно»).
 */
export function hintLine(t: TFunction, h: TorrentHint | undefined): string {
  if (!h) return ''
  if (h.stalled) return trackerLine(t, h.trackerErrorKind, h.trackerError) || stalledLine(t, h.stalled)
  return h.liveness === 'unseen' ? t('comp.stalled.unseen') : ''
}
