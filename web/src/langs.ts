// The language catalog of the release preferences (a copy of the core's
// components/langs.ts).
import { tr, type Key } from './i18n'
import type { LangCode } from './types'

/** The language catalog (internal/lang), in the order «Языки» offers it. */
export const LANGS: LangCode[] = ['ru', 'en', 'uk', 'de', 'fr', 'es', 'it', 'pt', 'pl', 'ja', 'zh', 'ko']
/** «Русский», «Английский» … in the interface language. */
export const langLabel = (c: LangCode) => (LANGS.includes(c) ? tr(`common.lang.${c}` as Key) : c)
