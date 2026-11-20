// Under a «Переезд» card whose files are not mounted: reach the service
// over the network instead (MigrateFullApi.SourceSetup). Sonarr, Radarr and
// Prowlarr ask for an address and an API key, qBittorrent for an address,
// a login and a password. «Подключить» checks them on the server, which
// keeps them (sealed in the addon's settings: a restart keeps them too) and counts what it found; the
// card then counts as found and the usual preview and run follow. A card
// with its files mounted draws nothing. The key and the password are never
// answered back: a kept one shows as «Сохранён», and a blank field keeps it.
import { useState, type ComponentProps, type FormEvent } from 'react'
import { ApiError, ui, type MigrateFullApi } from '@antresol/addon-sdk'
import { migrateConnect, migrateForget } from '../api'
import { tr, useT } from '../i18n'
import type { MigrateConnect } from '../types'
import { blockedBy, canSubmit, keepsSecret, requestOf } from './connectForm'
import s from './SourceSetup.module.css'

const { Button, TextField } = ui

type SetupProps = ComponentProps<NonNullable<MigrateFullApi['SourceSetup']>>

/** Where each service usually listens: the address field's example. */
const EXAMPLE: Record<string, string> = {
  prowlarr: 'http://prowlarr:9696',
  sonarr: 'http://sonarr:8989',
  radarr: 'http://radarr:7878',
  qbittorrent: 'http://qbittorrent:8080',
}

const errorText = (e: unknown) => (e instanceof ApiError ? e.message : tr('migrate.failedRetry'))

export function SourceSetup({ ctx, source }: SetupProps) {
  const t = useT()
  // the addon's status carries a service's connection beside the core's fields
  const connect = (source as typeof source & { connect?: MigrateConnect }).connect
  const [editing, setEditing] = useState(false)
  const [url, setUrl] = useState(connect?.url ?? '')
  const [apiKey, setApiKey] = useState('')
  const [username, setUsername] = useState(connect?.username ?? '')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (!connect) return null // its files are mounted: they are used

  const fields = { url, apiKey, username, password }
  const login = connect.kind === 'login'
  const locked = !!ctx.running || ctx.busy !== '' || busy
  const kept = keepsSecret(connect, fields)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (locked || !canSubmit(connect, fields)) return
    setBusy(true)
    setError('')
    try {
      await migrateConnect(source.id, requestOf(connect, fields))
      setEditing(false)
      setApiKey('')
      setPassword('')
      ctx.reload()
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const forget = async () => {
    setBusy(true)
    setError('')
    try {
      await migrateForget(source.id)
      setUrl('')
      setUsername('')
      ctx.reload()
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  if (connect.connected && !editing) {
    return (
      <div className={s.connected} data-testid="migrate-connected">
        <span className={s.address}>{t('migrate.connect.connected', { url: connect.url })}</span>
        <span className={s.actions}>
          <Button size="small" variant="secondary" disabled={locked} onClick={() => setEditing(true)}>
            {t('migrate.connect.change')}
          </Button>
          <Button size="small" variant="secondary" disabled={locked} loading={busy} onClick={() => void forget()}>
            {t('migrate.connect.forget')}
          </Button>
        </span>
        {error && (
          <p className={s.error} role="alert">
            {error}
          </p>
        )}
      </div>
    )
  }

  const keptHint = kept ? t('migrate.connect.kept') : undefined
  // why «Подключить» waits: said under it, not left a mystery
  const blocked = blockedBy(connect, fields)
  const why = blocked === 'url' ? t('migrate.connect.needUrl', { name: source.name }) : blocked === 'apiKey' ? t('migrate.connect.needKey', { name: source.name }) : ''
  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} data-testid="migrate-connect" aria-label={source.name}>
      {!source.found && !connect.connected && <p className={s.first}>{t('migrate.connect.first', { name: source.name })}</p>}
      <TextField
        label={t('migrate.connect.url')}
        type="url"
        inputMode="url"
        value={url}
        placeholder={EXAMPLE[source.id]}
        onChange={(e) => setUrl(e.target.value)}
        autoComplete="off"
        spellCheck={false}
        disabled={locked}
      />
      {login ? (
        <div className={s.pair}>
          <TextField
            label={t('migrate.connect.username')}
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            disabled={locked}
          />
          <TextField
            label={t('migrate.connect.password')}
            type="password"
            revealable
            value={password}
            placeholder={keptHint}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            disabled={locked}
          />
        </div>
      ) : (
        <TextField
          label={t('migrate.connect.apiKey')}
          type="password"
          revealable
          value={apiKey}
          placeholder={keptHint}
          hint={t('migrate.connect.apiKeyHint', { name: source.name })}
          onChange={(e) => setApiKey(e.target.value)}
          autoComplete="new-password"
          spellCheck={false}
          disabled={locked}
        />
      )}
      {error && (
        <p className={s.error} role="alert">
          {error}
        </p>
      )}
      <div className={s.submit}>
        <Button type="submit" size="small" loading={busy} disabled={locked || !canSubmit(connect, fields)}>
          {t('migrate.connect.submit')}
        </Button>
        {why && !locked && <span className={s.why}>{why}</span>}
        {editing && (
          <Button size="small" variant="secondary" disabled={busy} onClick={() => setEditing(false)}>
            {t('common.cancel')}
          </Button>
        )}
      </div>
    </form>
  )
}
