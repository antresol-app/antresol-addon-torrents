// «Настройки загрузок» (admins): the downloads part of the old build's
// «Настройки сервера», on the addon's own x/admin/settings — the
// release-search preferences (video quality, voices, size; book, audiobook
// and comic languages; music format and MP3 bitrate), and «Раздача и место»
// (incoming connections, «Не раздавать», the upload limit, the watch cache).
// Saved as they change (a number field after a pause). «Не раздавать» has its
// own endpoint and is locked, with «Задано на сервере», when the server
// forces it (ANTRESOL_TORRENTS_NO_SEEDING=on).
import { ApiError, Icon, ui } from '@antresol/addon-sdk'
import { useCallback, useEffect, useRef, useState } from 'react'
import { getEngine, getSettings, saveNoSeeding, saveSettings } from '../api'
import { useT } from '../i18n'
import { LANGS, langLabel } from '../langs'
import { RankedList } from '../RankedList'
import { setTitle } from '../title'
import type { AdminSettings, EngineStatus, LangCode, MusicFormat, Mp3Bitrate, Voice } from '../types'
import m from './Manage.module.css'
import styles from './Settings.module.css'

const { GroupedSection, ListRow, MultiSelect, NumberField, SegmentedControl, Select, Toggle } = ui

export const SIZE_STOPS = [2, 4, 6, 8, 10, 15, 20, 30, 40, 60, 80, 0]

/** The index of the stop nearest to gb (0 = no limit is the last). */
export function sizeStop(gb: number): number {
  if (gb <= 0) return SIZE_STOPS.length - 1
  let best = 0
  for (let i = 0; i < SIZE_STOPS.length - 1; i++) if (Math.abs(SIZE_STOPS[i]! - gb) < Math.abs(SIZE_STOPS[best]! - gb)) best = i
  return best
}

const errorText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback)

export function Settings() {
  const t = useT()
  const [values, setValues] = useState<AdminSettings | null>(null)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState<'' | 'saving' | 'saved'>('')
  const timer = useRef<number | undefined>(undefined)
  const latest = useRef<AdminSettings | null>(null)
  const pending = useRef(false)
  // «Не раздавать» has its own state: from GET and from its own PUT only,
  // never from the shared object a settings save writes and answers
  const [noSeed, setNoSeed] = useState<{ on: boolean; forced: boolean }>({ on: false, forced: false })

  useEffect(() => {
    setTitle(t('tsettings.title'))
    const ac = new AbortController()
    getSettings(ac.signal).then(
      (v) => {
        latest.current = v
        setValues(v)
        setNoSeed({ on: v.noSeeding, forced: v.noSeedingForced })
      },
      (e: unknown) => !ac.signal.aborted && setError(errorText(e, t('settings.loadFailed'))),
    )
    return () => ac.abort()
  }, [])

  const flush = useCallback(async () => {
    const v = latest.current
    if (!v) return
    setSaved('saving')
    try {
      const out = await saveSettings(v)
      setError('')
      setSaved('saved')
      // keep what the person is typing; only take the server's view when nothing changed meanwhile
      if (latest.current === v) {
        latest.current = out
        setValues(out)
      }
    } catch (e) {
      setSaved('')
      setError(errorText(e, t('settings.saveFailed')))
    }
  }, [])

  const change = (patch: Partial<AdminSettings>, wait = 0) => {
    if (!latest.current) return
    const next = { ...latest.current, ...patch }
    latest.current = next
    setValues(next)
    window.clearTimeout(timer.current)
    pending.current = true
    timer.current = window.setTimeout(() => {
      pending.current = false
      void flush()
    }, wait)
  }
  // leaving with a change still waiting (a number field's pause): send it
  useEffect(
    () => () => {
      window.clearTimeout(timer.current)
      if (pending.current) void flush()
    },
    [flush],
  )

  if (!values) {
    return (
      <section>
        <h2 className={m.title}>{t('tsettings.title')}</h2>
        {error ? (
          <div className={m.problem} role="alert" style={{ marginTop: 16 }}>
            {error}
          </div>
        ) : (
          <div className={m.spinnerRow} role="status">
            <span className={m.spinner} aria-hidden /> {t('settings.loading')}
          </div>
        )}
      </section>
    )
  }

  const stop = sizeStop(values.maxSizeGB)
  const musicFormat: MusicFormat = values.musicFormat ?? (values.preferLossless === false ? 'mp3' : 'lossless')
  const mbs = values.uploadLimitKBs / 1024
  const languages: LangCode[] = values.languages?.length ? values.languages : LANGS
  const mine = languages.map((c) => ({ value: c, label: langLabel(c) }))
  const voices: Voice[] = values.voices?.length ? values.voices : [values.voice]
  const voiceOptions: { value: Voice; label: string }[] = [
    ...(languages.includes('ru') ? [{ value: 'dub' as const, label: t('settings.voice.dub') }] : []),
    { value: 'original', label: t('common.original') },
  ]
  const sizeLabel = (gb: number) => (gb <= 0 ? t('settings.size.noLimit') : t('settings.size.upTo', { gb }))
  const kindLangs = (key: 'bookLangs' | 'audiobookLangs' | 'comicLangs', label: string) => (
    <ListRow stack label={label} hint={t('settings.kindLangs.hint')}>
      <MultiSelect
        label={label}
        placement="bottom-end"
        options={mine}
        value={languages.filter((c) => (values[key] ?? []).includes(c))}
        none={t('settings.kindLangs.none')}
        onChange={(v) => change({ [key]: v as LangCode[] })}
      />
    </ListRow>
  )

  return (
    <section aria-labelledby="tsettings-title" className={styles.page}>
      <header className={m.header}>
        <div>
          <h2 id="tsettings-title" className={m.title}>
            {t('tsettings.title')}
          </h2>
          <p className={m.subtitle} aria-live="polite">
            {saved === 'saving' ? t('settings.saving') : saved === 'saved' ? t('settings.saved') : t('settings.autosave')}
          </p>
        </div>
      </header>
      {error && (
        <div className={m.problem} role="alert">
          {error}
        </div>
      )}

      <GroupedSection title={t('settings.newVideo')}>
        <ListRow stack label={t('settings.videoQuality')} hint={t('tsettings.videoQuality.hint')}>
          <SegmentedControl
            label={t('settings.videoQuality')}
            segments={[
              { value: '720', label: '720p' },
              { value: '1080', label: '1080p' },
              { value: '2160', label: '4K' },
            ]}
            value={String(values.quality)}
            onChange={(v) => change({ quality: Number(v) as AdminSettings['quality'] })}
          />
        </ListRow>
        <ListRow wide label={t('settings.voices')} hint={t('settings.voices.hint')} id="voices">
          <RankedList
            labelledBy="voices"
            items={voices}
            itemLabel={(v) => voiceOptions.find((o) => o.value === v)?.label ?? v}
            min={1}
            onChange={(next) => change({ voices: next as Voice[], voice: next[0] as Voice })}
            choices={() => voiceOptions.filter((o) => !voices.includes(o.value))}
            onAdd={(v) => change({ voices: [...voices, v as Voice], voice: voices[0] ?? (v as Voice) })}
            addLabel={t('settings.voices.add')}
          />
        </ListRow>
        <ListRow stack label={t('settings.maxSize')} hint={t('settings.maxSize.hint')}>
          {/* a dozen fixed sizes: a pop-up, not a slider that only looks continuous */}
          <Select
            label={t('settings.maxSize')}
            placement="bottom-end"
            value={String(SIZE_STOPS[stop])}
            options={SIZE_STOPS.map((gb) => ({ value: String(gb), label: sizeLabel(gb) }))}
            onChange={(v) => change({ maxSizeGB: Number(v) })}
          />
        </ListRow>
      </GroupedSection>

      <GroupedSection title={t('settings.booksGroup')}>
        {kindLangs('bookLangs', t('settings.bookLangs'))}
        {kindLangs('audiobookLangs', t('settings.audiobookLangs'))}
        {kindLangs('comicLangs', t('settings.comicLangs'))}
      </GroupedSection>

      <GroupedSection title={t('tsettings.musicGroup')}>
        <ListRow stack label={t('settings.musicFormat')} hint={t('settings.musicFormat.hint')}>
          <SegmentedControl
            label={t('settings.musicFormat')}
            segments={[
              { value: 'lossless', label: t('settings.musicFormat.lossless') },
              { value: 'mp3', label: 'MP3' },
              { value: 'any', label: t('settings.musicFormat.any') },
            ]}
            value={musicFormat}
            onChange={(v) => change({ musicFormat: v as MusicFormat, preferLossless: v === 'lossless' })}
          />
        </ListRow>
        {musicFormat === 'mp3' && (
          <ListRow stack label={t('settings.bitrate')} hint={t('settings.bitrate.hint')}>
            <Select
              label={t('settings.bitrate')}
              placement="bottom-end"
              options={[
                { value: '320', label: t('settings.bitrate.320') },
                { value: 'v0', label: 'V0 (VBR)' },
                { value: '256', label: t('settings.bitrate.256') },
                { value: 'any', label: t('settings.bitrate.any') },
              ]}
              value={values.mp3Bitrate ?? '320'}
              onChange={(v) => change({ mp3Bitrate: v as Mp3Bitrate })}
            />
          </ListRow>
        )}
      </GroupedSection>

      <GroupedSection title={t('settings.seedingGroup')}>
        <IncomingRow />
        <NoSeedingRow
          on={noSeed.on}
          forced={noSeed.forced}
          onSaved={(on) => setNoSeed((s) => ({ ...s, on }))}
          onForced={() => setNoSeed({ on: true, forced: true })}
        />
        <ListRow label={t('settings.uploadLimit')} hint={t('settings.uploadLimit.hint')}>
          {values.uploadLimited && <UploadLimitField mbs={mbs} onChange={(v) => change({ uploadLimitKBs: Math.round(v * 1024) }, 700)} />}
          <Toggle
            label={t('settings.uploadLimit.toggle')}
            checked={values.uploadLimited}
            onChange={(on) => change({ uploadLimited: on, uploadLimitKBs: values.uploadLimitKBs > 0 ? values.uploadLimitKBs : 2048 })}
          />
        </ListRow>
        <ListRow label={t('settings.streamCache')} hint={t('settings.streamCache.hint')}>
          <NumberField
            label={t('settings.streamCache.aria')}
            unit={t('common.unit.GB')}
            min={1}
            value={Math.round(values.streamCacheGB)}
            onChange={(v) => change({ streamCacheGB: v }, 700)}
          />
        </ListRow>
        {(values.outsideFileRoots ?? 0) > 0 && (
          <ListRow
            label={
              <>
                <Icon name="warning" size={16} className={styles.warnIcon} /> {t('tsettings.outside', { count: values.outsideFileRoots ?? 0 })}
              </>
            }
          />
        )}
      </GroupedSection>
    </section>
  )
}

/**
 * «Не раздавать»: saved on its own endpoint at once. Forced by the server:
 * shown on, disabled, with «Задано на сервере»; nothing is sent.
 */
export function NoSeedingRow({ on, forced, onSaved, onForced }: { on: boolean; forced: boolean; onSaved: (on: boolean) => void; onForced?: () => void }) {
  const t = useT()
  const [value, setValue] = useState(forced || on)
  const [error, setError] = useState('')
  // one request at a time, and the latest wins: an older answer never moves the toggle
  const saving = useRef(false)
  const seq = useRef(0)
  useEffect(() => setValue(forced || on), [on, forced])
  const change = async (next: boolean) => {
    if (forced || saving.current) return
    saving.current = true
    const mine = ++seq.current
    setValue(next)
    try {
      await saveNoSeeding(next)
      if (mine !== seq.current) return
      setError('')
      onSaved(next)
    } catch (e) {
      if (mine !== seq.current) return
      setError(errorText(e, t('settings.saveFailed')))
      // forced since the page loaded (the server restarted with the override): on and locked
      if (e instanceof ApiError && e.status === 409) {
        setValue(true)
        onForced?.()
      } else setValue(on)
    } finally {
      saving.current = false
    }
  }
  return (
    <ListRow
      id="settings-no-seeding"
      label={t('settings.noSeeding')}
      hint={forced ? t('tsettings.noSeeding.forced') : t('settings.noSeeding.hint')}
      status={error || undefined}
    >
      <Toggle label={t('settings.noSeeding')} checked={value} disabled={forced} onChange={(v) => void change(v)} />
    </ListRow>
  )
}

/**
 * «Входящие подключения»: only when the engine sees that no peer has ever
 * connected in, with the port to open. Read once; nothing while it is fine
 * or unknown.
 */
function IncomingRow() {
  const [st, setSt] = useState<EngineStatus | null>(null)
  const t = useT()
  useEffect(() => {
    const ac = new AbortController()
    getEngine(ac.signal).then(setSt, () => {})
    return () => ac.abort()
  }, [])
  if (!st?.incomingBlocked) return null
  return (
    <ListRow
      label={
        <>
          <Icon name="warning" size={16} className={styles.warnIcon} /> {t('settings.incoming')}
        </>
      }
      status={t('settings.incoming.blocked', { port: st.listenPort ?? 0 })}
    />
  )
}

/** The upload limit in МБ/с (more than 0; a hundredth at the finest). */
export function UploadLimitField({ mbs, onChange }: { mbs: number; onChange: (v: number) => void }) {
  const t = useT()
  return (
    <NumberField
      label={t('settings.uploadLimit.aria')}
      unit={t('common.perSecond', { size: t('common.unit.MB') })}
      value={mbs}
      min={0.01}
      decimals={2}
      onChange={onChange}
    />
  )
}
