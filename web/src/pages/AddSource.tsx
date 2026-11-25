// «Добавить источник»: find the site in the catalog (Russian trackers
// first), fill in only what the site needs — with a hint under every field
// — and press «Подключить». Vault saves the source and checks it at once:
// «Подключено ✓» or the server's explanation. Everything optional waits
// under «Дополнительно».
import { ApiError, Icon, locale, ui } from '@antresol/addon-sdk'
import { addSource, getCatalog, getCatalogEntry, getSources, testSource, updateSource } from '../api'
import { type CatalogEntry, type CatalogField, type Source } from '../types'
import { languageLabel } from './SourceCard'
import { lang as getLanguage, tr, useT, type Key } from '../i18n'
import { setTitle } from '../title'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import m from './Manage.module.css'
import styles from './Sources.module.css'

const { Button, Select, Spinner, TextArea, TextField, Toggle } = ui

/** The trackers most people here want, in this order. */
export const POPULAR = ['rutracker', 'rutracker-org-cookie', 'rutor', 'kinozal', 'noname-club', 'opds']

const SHOWN = 60

const isRussian = (e: CatalogEntry) => e.language.toLowerCase().startsWith('ru')

/** Popular first, then Russian-language sites, then the rest (server order within each). */
export function orderCatalog(list: CatalogEntry[]): CatalogEntry[] {
  const rank = (e: CatalogEntry) => {
    const p = POPULAR.indexOf(e.id)
    if (p >= 0) return p
    return isRussian(e) ? POPULAR.length : POPULAR.length + 1
  }
  return list
    .map((e, i) => ({ e, i }))
    .sort((a, b) => rank(a.e) - rank(b.e) || a.i - b.i)
    .map((x) => x.e)
}

/** Our blurbs for the trackers people choose most; other descriptions
 * are shown only when they are in the interface language (most definitions are English). */
const BLURBS: Record<string, Key> = {
  rutracker: 'sources.blurb.rutracker',
  'rutracker-org-cookie': 'sources.blurb.rutrackerCookie',
  rutor: 'sources.blurb.rutor',
  kinozal: 'sources.blurb.kinozal',
  'noname-club': 'sources.blurb.nonameClub',
  opds: 'sources.blurb.opds',
}

export function describe(e: CatalogEntry): string {
  const blurb = BLURBS[e.id]
  if (blurb) return tr(blurb)
  const russian = /[а-яё]/i.test(e.description)
  return (getLanguage() === 'ru') === russian ? e.description : ''
}

const ACCESS: Record<string, Key> = { public: 'sources.access.public', 'semi-private': 'sources.access.semiPrivate', private: 'sources.access.private' }
const access = (type: string) => (ACCESS[type] ? tr(ACCESS[type]) : '')

export function AddSource() {
  const { id } = useParams()
  return id ? <SourceForm id={id} /> : <CatalogSearch />
}

/** /addons/torrents/sources/:sourceId/edit — the form of a saved source (fix a login, a password, an expired cookie). */
export function EditSource() {
  const t = useT()
  const { sourceId } = useParams()
  const [source, setSource] = useState<Source | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    const ac = new AbortController()
    getSources(ac.signal).then(
      (list) => {
        const s = list.find((x) => String(x.id) === sourceId)
        if (s) setSource(s)
        else setError(tr('sources.edit.notFound'))
      },
      (e: unknown) => !ac.signal.aborted && setError(e instanceof ApiError ? e.message : tr('sources.edit.loadFailed')),
    )
    return () => ac.abort()
  }, [sourceId])
  if (error) {
    return (
      <section>
        <Link to="/addons/torrents/sources" className={m.back}>
          <Icon name="chevronLeft" size={18} strokeWidth={2.4} />
          {t('sources.title')}
        </Link>
        <div className={m.problem} role="alert">
          {error}
        </div>
      </section>
    )
  }
  if (!source) {
    return (
      <div className={m.spinnerRow} role="status">
        <Spinner track className={m.spinner} /> {t('sources.loading')}
      </div>
    )
  }
  return <SourceForm id={source.definitionId} editing={source} />
}

function CatalogSearch() {
  const t = useT()
  const [q, setQ] = useState('')
  const [list, setList] = useState<CatalogEntry[] | null>(null)
  const [error, setError] = useState('')
  const [all, setAll] = useState(false)
  const input = useRef<HTMLInputElement>(null)

  useEffect(() => {
    setTitle(tr('sources.add.title'))
    input.current?.focus()
  }, [])
  useEffect(() => {
    const ac = new AbortController()
    const timer = window.setTimeout(() => {
      getCatalog(q.trim(), ac.signal).then(
        (l) => {
          setList(orderCatalog(l))
          setError('')
        },
        (e: unknown) => !ac.signal.aborted && setError(e instanceof ApiError ? e.message : tr('sources.add.catalogFailed')),
      )
    }, 200)
    return () => {
      window.clearTimeout(timer)
      ac.abort()
    }
  }, [q])

  const shown = list ? (all ? list : list.slice(0, SHOWN)) : []
  return (
    <section aria-labelledby="add-title">
      <header className={m.header}>
        <div>
          <Link to="/addons/torrents/sources" className={m.back}>
            <Icon name="chevronLeft" size={18} strokeWidth={2.4} />
            {t('sources.title')}
          </Link>
          <h2 id="add-title" className={m.title}>
            {t('sources.add.title')}
          </h2>
          <p className={m.subtitle}>{t('sources.add.subtitle')}</p>
        </div>
      </header>
      <TextField
        ref={input}
        type="search"
        className={styles.search}
        label={t('sources.add.searchAria')}
        hideLabel
        placeholder={t('sources.add.placeholder')}
        value={q}
        onChange={(e) => (setQ(e.target.value), setAll(false))}
        onClear={() => (setQ(''), setAll(false))}
        spellCheck={false}
      />
      {error && (
        <div className={m.problem} role="alert">
          {error}
        </div>
      )}
      {!list && !error && (
        <div className={m.spinnerRow} role="status">
          <Spinner track className={m.spinner} /> {t('sources.add.loadingSites')}
        </div>
      )}
      {list && list.length === 0 && <p className={m.groupFoot}>{t('sources.add.noSuchSite')}</p>}
      {shown.length > 0 && (
        <ul className={styles.catalog} aria-label={t('sources.add.sitesAria')}>
          {shown.map((e) => (
            <li key={e.id}>
              <Link to={`/addons/torrents/sources/add/${encodeURIComponent(e.id)}`} className={styles.entry}>
                <div className={styles.entryText}>
                  <p className={styles.entryName}>{e.name}</p>
                  {describe(e) && <p className={styles.entryDesc}>{describe(e)}</p>}
                </div>
                <span className={styles.entryMeta}>{[languageLabel(e.language), access(e.type)].filter(Boolean).join(' · ')}</span>
                <Icon name="chevronRight" size={14} strokeWidth={2.4} className={styles.chevron} />
              </Link>
            </li>
          ))}
        </ul>
      )}
      {list && !all && list.length > SHOWN && (
        <p className={m.groupFoot}>
          <Button size="small" variant="plain" onClick={() => setAll(true)}>
            {t('sources.add.showAll', { n: list.length })}
          </Button>
        </p>
      )}
    </section>
  )
}

/** Help text: numbered lines become a list of steps. */
function Help({ text, id }: { text: string; id: string }) {
  const lines = text.split('\n').map((l) => l.trim()).filter(Boolean)
  const steps = lines.filter((l) => /^\d+\.\s/.test(l))
  if (steps.length < 2) {
    return (
      <p id={id} className={styles.fieldHelp}>
        {text}
      </p>
    )
  }
  const before = lines.slice(0, lines.indexOf(steps[0]!))
  const after = lines.slice(lines.indexOf(steps[steps.length - 1]!) + 1)
  return (
    <div id={id} className={styles.fieldHelp}>
      {before.map((l) => (
        <p key={l}>{l}</p>
      ))}
      <ol className={styles.steps}>
        {steps.map((l) => (
          <li key={l}>{l.replace(/^\d+\.\s*/, '')}</li>
        ))}
      </ol>
      {after.map((l) => (
        <p key={l} className={styles.helpAfter}>
          {l}
        </p>
      ))}
    </div>
  )
}

function Field({ f, value, onChange }: { f: CatalogField; value: string; onChange: (v: string) => void }) {
  const t = useT()
  const helpId = `help-${f.name}`
  const type = f.type.toLowerCase()
  if (type === 'checkbox') {
    const on = ['true', '1', 'on', 'yes'].includes(value.toLowerCase())
    return (
      <div>
        <div className={styles.checkRow}>
          <span className={m.rowLabel}>{f.label}</span>
          <Toggle label={f.label} checked={on} describedBy={f.help ? helpId : undefined} onChange={(v) => onChange(v ? 'true' : 'false')} />
        </div>
        {f.help && <Help text={f.help} id={helpId} />}
      </div>
    )
  }
  if (type === 'select' && f.options?.length) {
    return (
      <div>
        <p className={styles.label} id={`f-${f.name}`}>
          {f.label}
        </p>
        <Select label={f.label} className={styles.select} value={value} options={f.options.map((o) => ({ value: o.value, label: o.label }))} onChange={onChange} />
        {f.help && <Help text={f.help} id={helpId} />}
      </div>
    )
  }
  // Long secrets (cookie) are pasted, not typed: a text area.
  if (f.name.toLowerCase() === 'cookie') {
    return (
      <div>
        <TextArea
          id={`f-${f.name}`}
          label={f.label}
          rows={3}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={t('sources.form.cookiePlaceholder')}
          aria-describedby={f.help ? helpId : undefined}
          spellCheck={false}
          autoComplete="off"
        />
        {f.help && <Help text={f.help} id={helpId} />}
      </div>
    )
  }
  return (
    <div>
      <TextField
        label={f.label}
        type={f.secret || type === 'password' ? 'password' : 'text'}
        revealable={f.secret || type === 'password'}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
      {f.help && <Help text={f.help} id={helpId} />}
    </div>
  )
}

type Phase = { status: 'idle' } | { status: 'busy' } | { status: 'ok'; source: Source } | { status: 'error'; message: string }

function SourceForm({ id, editing }: { id: string; editing?: Source }) {
  const t = useT()
  const navigate = useNavigate()
  const [entry, setEntry] = useState<CatalogEntry | null>(null)
  const [loadError, setLoadError] = useState('')
  const [values, setValues] = useState<Record<string, string>>({})
  const [phase, setPhase] = useState<Phase>({ status: 'idle' })
  const saved = useRef<Source | null>(editing ?? null)

  useEffect(() => {
    const ac = new AbortController()
    getCatalogEntry(id, ac.signal).then(
      (e) => {
        setEntry(e)
        setTitle(e.name)
        // Editing: the saved values (secrets come back as «••••••», which
        // the server reads as "keep").
        setValues(Object.fromEntries((e.fields ?? []).map((f) => [f.name, editing?.settings[f.name] ?? (editing ? '' : (f.default ?? ''))])))
      },
      (e: unknown) => !ac.signal.aborted && setLoadError(e instanceof ApiError ? e.message : tr('sources.form.entryFailed')),
    )
    return () => ac.abort()
  }, [id, editing])

  const fields = useMemo(() => entry?.fields ?? [], [entry])
  const required = fields.filter((f) => f.required || !f.advanced)
  const advanced = fields.filter((f) => !(f.required || !f.advanced))
  const missing = required.filter((f) => f.required && !values[f.name]?.trim())

  const connect = async () => {
    if (!entry) return
    setPhase({ status: 'busy' })
    // A cleared field removes the setting from a saved source.
    const settings = editing ? { ...values } : Object.fromEntries(Object.entries(values).filter(([, v]) => v !== ''))
    try {
      // A second try after an error updates the source saved the first time.
      let src: Source
      if (saved.current) {
        await updateSource(saved.current.id, { settings })
        src = await testSource(saved.current.id)
      } else {
        src = await addSource(entry.id, settings)
      }
      saved.current = src
      if (src.status === 'ok') setPhase({ status: 'ok', source: src })
      else
        setPhase({
          status: 'error',
          message: t('sources.form.savedFix', {
            reason: (src.lastError || t('sources.form.noAnswer')).replace(/[.!]?$/, '.'),
            button: editing ? t('common.save') : t('sources.form.connect'),
          }),
        })
    } catch (e) {
      setPhase({ status: 'error', message: e instanceof ApiError ? e.message : t('sources.form.connectFailed') })
    }
  }

  if (loadError) {
    return (
      <section>
        <Link to="/addons/torrents/sources/add" className={m.back}>
          <Icon name="chevronLeft" size={18} strokeWidth={2.4} />
          {t('sources.form.allSites')}
        </Link>
        <div className={m.problem} role="alert">
          {loadError}
        </div>
      </section>
    )
  }
  if (!entry) {
    return (
      <div className={m.spinnerRow} role="status">
        <Spinner track className={m.spinner} /> {t('sources.loading')}
      </div>
    )
  }

  const set = (name: string) => (v: string) => {
    setValues((s) => ({ ...s, [name]: v }))
    if (phase.status === 'error') setPhase({ status: 'idle' })
  }
  const busy = phase.status === 'busy'
  return (
    <section aria-labelledby="form-title">
      <Link to={editing ? '/addons/torrents/sources' : '/addons/torrents/sources/add'} className={m.back}>
        <Icon name="chevronLeft" size={18} strokeWidth={2.4} />
        {editing ? t('sources.title') : t('sources.form.allSites')}
      </Link>
      <form
        className={styles.form}
        onSubmit={(e) => {
          e.preventDefault()
          if (!busy && missing.length === 0) void connect()
        }}
      >
        <div className={styles.formHead}>
          <span className={styles.formLogo} aria-hidden>
            {entry.name.charAt(0).toLocaleUpperCase(locale())}
          </span>
          <div>
            <h2 id="form-title" className={m.title}>
              {editing ? t('sources.form.editTitle', { name: editing.name }) : entry.name}
            </h2>
            <p className={styles.formDesc}>{[languageLabel(entry.language), access(entry.type)].filter(Boolean).join(' · ')}</p>
          </div>
        </div>
        {describe(entry) && <p className={styles.formDesc}>{describe(entry)}</p>}

        {required.length === 0 && <p className={styles.noFields}>{t('sources.form.noFields')}</p>}
        {required.map((f) => (
          <Field key={f.name} f={f} value={values[f.name] ?? ''} onChange={set(f.name)} />
        ))}

        {advanced.length > 0 && (
          <details className={styles.advanced}>
            <summary>
              <Icon name="chevronRight" size={14} strokeWidth={2.4} />
              <span className={styles.advancedLabel}>{t('sources.form.advanced')}</span>
            </summary>
            <div className={styles.advancedBody}>
              {advanced.map((f) => (
                <Field key={f.name} f={f} value={values[f.name] ?? ''} onChange={set(f.name)} />
              ))}
            </div>
          </details>
        )}

        {phase.status === 'error' && (
          <p className={styles.failed} role="alert">
            {phase.message}
          </p>
        )}
        <div className={styles.connect}>
          {phase.status === 'ok' ? (
            <>
              <span className={styles.connected} role="status">
                {t('sources.form.connected')}
              </span>
              <Button onClick={() => navigate('/addons/torrents/sources')}>{t('common.done')}</Button>
            </>
          ) : (
            <Button type="submit" size="large" loading={busy} disabled={missing.length > 0}>
              {busy ? (editing ? t('sources.form.saving') : t('sources.form.connecting')) : editing ? t('common.save') : t('sources.form.connect')}
            </Button>
          )}
          {phase.status !== 'ok' && missing.length > 0 && <span className={m.rowHint}>{t('sources.form.fill', { fields: missing.map((f) => f.label).join(', ') })}</span>}
        </div>
      </form>
    </section>
  )
}
