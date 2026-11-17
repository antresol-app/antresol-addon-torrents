// The input player's source progress for a «Смотреть сейчас» input
// (playerNotes' useProgress): the bytes the download has plus what arrived
// where the player reads next. It only grows: the player keeps waiting
// while it does.
import type { PlayerInput } from '@antresol/addon-sdk'
import { useRef } from 'react'
import { useWatchStatus, watchOf } from './status'

export function useWatchProgress(input: PlayerInput): number {
  const status = useWatchStatus(watchOf(input))
  const most = useRef(0)
  if (status) most.current = Math.max(most.current, status.done + (status.bufferHave ?? 0))
  return most.current
}
