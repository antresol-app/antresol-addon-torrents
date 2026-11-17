// «Найти и добавить» as «Поиск»'s block (searchBlocks): the page hands it
// the query and its filter; the signed-in profile decides what the admin
// gets (picking a release of an album, the track step). The core never
// renders it for a kid (useAddonSlot). An empty library page's «Найти и
// добавить» (emptyStateActions) opens /search?dk=<kind>: that catalog,
// until the page's filter is changed (it drops ?dk=).
import { useUser, type SearchBlock } from '@antresol/addon-sdk'
import { useSearchParams } from 'react-router'
import type { DiscoverKind } from '../types'
import { FindAndAdd } from './FindAndAdd'

const KINDS: readonly DiscoverKind[] = ['video', 'music', 'book', 'audiobook', 'comic']

type Props = Parameters<SearchBlock['element']>[0]

export function FindAndAddBlock({ q, scope, text, nothing }: Props) {
  const user = useUser()
  const [params] = useSearchParams()
  const dk = params.get('dk')
  const kind = KINDS.find((k) => k === dk)
  return <FindAndAdd text={text} q={q} scope={scope} nothing={nothing} admin={Boolean(user?.isAdmin)} kind={kind} />
}
