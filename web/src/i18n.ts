// The addon's strings in the interface language: t(key, vars) over its own
// catalogs (strings.ts), with {name} placeholders and plural entries (picked
// by vars.count, Intl.PluralRules), as the core's own t. useT re-renders on a
// language change through the SDK's useAddonT; tr is for code outside React.
import { locale, useAddonT } from '@antresol/addon-sdk'
import { useMemo } from 'react'
import { ID } from './api'
import { catalogs, type Entry, type Key } from './strings'

export type { Key }
export type Lang = 'ru' | 'en'
type Vars = Record<string, string | number>

/** The interface language, from the SDK's locale (ru-RU / en-US). */
export const lang = (): Lang => (locale().startsWith('en') ? 'en' : 'ru')

const rules = new Map<Lang, Intl.PluralRules>()
function plural(l: Lang, forms: Exclude<Entry, string>, n: number): string {
  let r = rules.get(l)
  if (!r) rules.set(l, (r = new Intl.PluralRules(l === 'ru' ? 'ru-RU' : 'en-US')))
  const cat = r.select(n) as keyof typeof forms
  return forms[cat] ?? forms.other ?? forms.many ?? forms.one ?? ''
}

export function translate(l: Lang, key: Key, vars?: Vars): string {
  const entry: Entry | undefined = catalogs[l][key] ?? catalogs.ru[key]
  if (entry === undefined) return key
  const s = typeof entry === 'string' ? entry : plural(l, entry, Number(vars?.count ?? 0))
  return vars ? s.replace(/\{(\w+)\}/g, (m, name: string) => (name in vars ? String(vars[name]) : m)) : s
}

/** Outside React (or in a component that called useT). */
export const tr = (key: Key, vars?: Vars): string => translate(lang(), key, vars)

export type TFunction = (key: Key, vars?: Vars) => string

/** t bound to the interface language; a new function after a change. */
export function useT(): TFunction {
  const sdkT = useAddonT(ID) // subscribes to language changes
  return useMemo<TFunction>(() => {
    const l = lang()
    return (key, vars) => translate(l, key, vars)
  }, [sdkT])
}
