// «Оставить на сервере» in the input player's top bar (playerNotes'
// actions) while a «Смотреть сейчас» input plays: the download is kept in
// full and goes to the library (x/torrents/{ih}/promote, any adult).
import { ApiError, ui, type PlayerInput } from '@antresol/addon-sdk'
import { useState } from 'react'
import { promote } from '../api'
import { useT } from '../i18n'
import { useWatchStatus, watchOf } from './status'

const { Button } = ui

export function KeepAction({ input }: { input: PlayerInput }) {
  const w = watchOf(input)
  const status = useWatchStatus(w)
  if (!w) return null
  // the mode the download has now («download»: kept already), else what x/watch saw
  return <KeepButton infoHash={w.ih} mode={status?.mode || w.mode || undefined} />
}

type Keep = { status: 'idle' | 'busy' | 'kept' | 'waiting' } | { status: 'error'; message: string }

/**
 * «Оставить на сервере»: the torrent is downloaded in full and added to the
 * library (promote). Any adult profile may do it; on a full disk it waits
 * for space.
 */
export function KeepButton({ infoHash, mode }: { infoHash: string; mode?: string }) {
  const t = useT()
  const [keep, setKeep] = useState<Keep>({ status: 'idle' })
  const already = mode !== undefined && mode !== 'stream'
  const state = keep.status === 'idle' && already ? 'kept' : keep.status
  const run = async () => {
    setKeep({ status: 'busy' })
    try {
      const r = await promote(infoHash)
      setKeep({ status: r.status === 'waiting_space' ? 'waiting' : 'kept' })
    } catch (e) {
      setKeep({ status: 'error', message: e instanceof ApiError ? e.message : t('player.stream.failedRetry') })
    }
  }
  const label =
    state === 'kept' ? t('player.stream.onServer') : state === 'waiting' ? t('common.download.waitingSpace') : state === 'error' ? t('common.failed') : t('player.stream.keep')
  const hint =
    state === 'kept'
      ? t('player.stream.keptHint')
      : state === 'waiting'
        ? t('player.stream.waitingHint')
        : keep.status === 'error'
          ? keep.message
          : t('player.stream.keepHint')
  return (
    <>
      <Button
        size="small"
        variant="secondary"
        icon={state === 'kept' || state === 'waiting' ? 'check' : 'download'}
        loading={state === 'busy'}
        disabled={state === 'kept' || state === 'waiting' || state === 'busy'}
        title={hint}
        onClick={() => void run()}
      >
        {label}
      </Button>
      <span className="visually-hidden" role="status">
        {state === 'kept' || state === 'waiting' || keep.status === 'error' ? hint : ''}
      </span>
    </>
  )
}
