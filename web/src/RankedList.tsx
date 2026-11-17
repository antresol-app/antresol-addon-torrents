// An ordered list of names, the way iOS and macOS Settings edit one
// (ui/SortableList: each row has its name, a remove button and a drag
// handle (≡) that drags with a mouse or a finger; ↑/↓ (Home/End) on the
// handle move the row and the new place is read out). «+ Добавить…» under the list opens a menu of
// what can still be added. The first row is the main one; the caller's hint
// says so. «Озвучка» uses it here.
// A copy of the core's components/RankedList.tsx (not in the SDK), on the
// SDK's ui, without the core's free-text field (studios), which no page of
// the addon needs.
import { ui } from '@antresol/addon-sdk'
import { useRef } from 'react'
import styles from './RankedList.module.css'

const { Button, MenuItem, Menu, SortableList } = ui

export interface RankedChoice {
  value: string
  label: string
}

export interface RankedListProps {
  /** The list's accessible name (or `labelledBy`). */
  label?: string
  labelledBy?: string
  items: string[]
  /** How an item reads (a language code → «Русский»); the item itself by default. */
  itemLabel?: (v: string) => string
  /** These many rows always stay (their remove button is off). */
  min?: number
  /** No «Добавить» once the list has this many. */
  max?: number
  /** A reorder or a removal. */
  onChange: (items: string[]) => void
  /** What the add menu offers (not in the list yet). */
  choices: () => RankedChoice[]
  /** A choice of the menu was added. */
  onAdd: (value: string) => void
  /** «Добавить язык»: the button and the menu's name. */
  addLabel: string
  /** Shown in place of an empty list. */
  empty?: string
}

export function RankedList({ label, labelledBy, items, itemLabel = (v) => v, min = 0, max, onChange, choices, onAdd, addLabel, empty }: RankedListProps) {
  const addRef = useRef<HTMLButtonElement>(null)

  const offered = choices()
  const full = max !== undefined && items.length >= max
  const canAdd = !full && offered.length > 0

  return (
    <div className={styles.box}>
      <SortableList
        label={label}
        labelledBy={labelledBy}
        items={items}
        getKey={(v) => v}
        itemLabel={itemLabel}
        renderItem={(v) => <span className={styles.name}>{itemLabel(v)}</span>}
        onChange={onChange}
        removable
        min={min}
        empty={empty && <p className={styles.empty}>{empty}</p>}
        focusWhenEmpty={() => addRef.current}
      />
      {canAdd && (
        <Menu
          label={addLabel}
          placement="bottom-start"
          focusItem
          trigger={(p) => (
            <Button
              {...p}
              ref={(el) => {
                addRef.current = el
                if (typeof p.ref === 'function') p.ref(el)
                else if (p.ref) (p.ref as { current: HTMLButtonElement | null }).current = el
              }}
              variant="plain"
              size="small"
              icon="plusCircle"
            >
              {addLabel}
            </Button>
          )}
        >
          {(close) => (
            <>
              {offered.map((c) => (
                <MenuItem
                  key={c.value}
                  label={c.label}
                  onSelect={() => {
                    onAdd(c.value)
                    close()
                  }}
                />
              ))}
            </>
          )}
        </Menu>
      )}
    </div>
  )
}
