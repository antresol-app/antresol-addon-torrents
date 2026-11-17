// «Управление → Просьбы»: «Одобрить» opens the release picker of «Найти и
// добавить» for the requested title (requestActions; the core opens the
// first visible one). The download it starts approves the request on the
// server; once the server took it, onApproved (the core reloads the list
// and closes the sheet). «Отмена» is onClose: nothing approved. A movie or
// a series gets the video sheet (its releases by TMDB id; a series has no
// «Лучшая раздача»: the server never picks a whole series, the admin picks
// a season or a pack), an album or book the media sheet.
import type { KidRequest } from '@antresol/addon-sdk'
import { useAcquire } from '../shared/useAcquire'
import type { Candidate } from '../types'
import { MediaReleaseSheet, VideoReleaseSheet } from './FindAndAdd'

/** The title of a request as «Найти и добавить» found it. */
export function requestCandidate(r: KidRequest): Candidate {
  return {
    tmdbId: r.tmdbId,
    extId: r.extId,
    kind: r.kind,
    title: r.title,
    subtitle: r.subtitle,
    year: r.year,
    poster: r.poster,
    overview: '',
    inLibrary: false,
    libraryItemId: null,
    aliases: r.aliases,
  }
}

export function RequestReleaseSheet({ request, onClose, onApproved }: { request: KidRequest; onClose: () => void; onApproved: () => void }) {
  const { state, run, reset } = useAcquire()
  const c = requestCandidate(request)
  const acq = { state, run, reset }
  if (request.kind === 'movie' || request.kind === 'series')
    return <VideoReleaseSheet c={c} mode="download" admin acq={acq} onStarted={(ok) => ok && onApproved()} onClose={onClose} />
  // the media sheet closes by itself once the download started: that close is the approval
  return <MediaReleaseSheet c={c} query="" admin owned={false} best acq={acq} onClose={() => (state.status === 'done' ? onApproved() : onClose())} />
}
