// «Статистика» in the input player's top bar (playerNotes' actions, beside
// «Оставить на сервере») while a «Смотреть сейчас» input plays: a sheet of
// the download as the poll sees it (useWatchStatus, the same one the note
// reads) — the release, its source, size, progress, speeds, peers and the
// buffer. Upload is shown as it is: «Не раздавать» gives nobody anything.
import { formatPercent, formatRate, formatSize, ui, type PlayerInput } from '@antresol/addon-sdk'
import { useState } from 'react'
import { torrentStatusLabel } from '../format'
import { useT, type TFunction } from '../i18n'
import type { StreamStatus } from '../types'
import { KeepButton } from './KeepAction'
import { useWatchStatus, watchOf } from './status'
import styles from './StatsAction.module.css'

const { Button, GroupedSection, ListRow, Sheet } = ui

/** The top bar's buttons of a «Смотреть сейчас» input: «Оставить на сервере», «Статистика». */
export function PlayerActions({ input }: { input: PlayerInput }) {
  const w = watchOf(input)
  const status = useWatchStatus(w)
  if (!w) return null
  return (
    <>
      <KeepButton infoHash={w.ih} mode={status?.mode || w.mode || undefined} />
      <StatsButton status={status} />
    </>
  )
}

export function StatsButton({ status }: { status: StreamStatus | null }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button size="small" variant="secondary" icon="chart" title={t('player.stats.hint')} onClick={() => setOpen(true)}>
        {t('player.stats')}
      </Button>
      {open && (
        <Sheet
          title={t('player.stats.title')}
          role="dialog"
          dismissOnOutside
          closeButton
          noButtons
          // full screen: the sheet joins the player, else it would draw behind it
          container={typeof document !== 'undefined' ? document.fullscreenElement : null}
          onCancel={() => setOpen(false)}
        >
          <StatsRows status={status} t={t} />
        </Sheet>
      )}
    </>
  )
}

const Value = ({ children }: { children: string }) => <span className={styles.value}>{children}</span>

/** The rows; «—» where nothing is known yet. */
export function StatsRows({ status: s, t }: { status: StreamStatus | null; t: TFunction }) {
  if (!s) return <p className={styles.empty}>{t('player.stream.connecting')}</p>
  const progress = s.size > 0 ? `${formatSize(s.done)} / ${formatSize(s.size)} · ${formatPercent(Math.min(100, (s.done / s.size) * 100))}` : '—'
  const buffer = (s.bufferWant ?? 0) > 0 ? `${formatSize(s.bufferHave ?? 0)} / ${formatSize(s.bufferWant ?? 0)}` : '—'
  const upload = s.noUpload ? t('player.stats.upload.off') : `${formatRate(s.upRate ?? 0)} · ${t('player.stats.uploaded', { size: formatSize(s.uploaded ?? 0) })}`
  return (
    <GroupedSection footer={s.noUpload ? t('player.stats.noUploadHint') : undefined}>
      <ListRow wide label={t('player.stats.release')}>
        <Value>{s.name || '—'}</Value>
      </ListRow>
      <ListRow label={t('player.stats.source')}>
        <Value>{s.sourceName || '—'}</Value>
      </ListRow>
      <ListRow label={t('player.stats.state')}>
        <Value>{torrentStatusLabel(s.status, s.mode)}</Value>
      </ListRow>
      <ListRow label={t('player.stats.size')}>
        <Value>{s.size > 0 ? formatSize(s.size) : '—'}</Value>
      </ListRow>
      <ListRow label={t('player.stats.progress')}>
        <Value>{progress}</Value>
      </ListRow>
      <ListRow label={t('player.stats.down')}>
        <Value>{formatRate(s.downRate)}</Value>
      </ListRow>
      <ListRow label={t('player.stats.up')}>
        <Value>{upload}</Value>
      </ListRow>
      <ListRow label={t('player.stats.peers')}>
        <Value>{t('player.stats.peersValue', { seeds: s.seeds, peers: s.peers })}</Value>
      </ListRow>
      <ListRow label={t('player.stats.buffer')} hint={t('player.stats.bufferHint')}>
        <Value>{buffer}</Value>
      </ListRow>
    </GroupedSection>
  )
}
