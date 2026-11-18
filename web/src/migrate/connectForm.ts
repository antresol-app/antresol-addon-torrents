// The logic of a «Переезд» card's connection form (SourceSetup.tsx), kept
// apart so it is tested without the page: what the form may send, and when a
// key or a password that was kept may stay blank.
import type { MigrateConnect, MigrateConnectRequest } from '../types'

/** What the person has typed. */
export interface ConnectFields {
  url: string
  apiKey: string
  username: string
  password: string
}

/** The address as the server keeps it: trimmed, without a trailing slash. */
export const cleanAddress = (url: string) => url.trim().replace(/\/+$/, '')

/**
 * Whether the kept key (or password) is still the one to use: it is kept,
 * and the address (for qBittorrent also the login) is the one it was given
 * for. The server applies the same rule, so a blank field is then enough.
 */
export function keepsSecret(c: MigrateConnect, f: ConnectFields): boolean {
  if (!c.hasSecret || cleanAddress(f.url) !== c.url) return false
  return c.kind === 'login' ? f.username.trim() === (c.username ?? '') : true
}

/**
 * «Подключить» may be pressed: an address, and a key (Sonarr, Radarr,
 * Prowlarr) typed or kept. qBittorrent may have no password (its web
 * interface can let the local network in), so its form needs the address only.
 */
export function canSubmit(c: MigrateConnect, f: ConnectFields): boolean {
  if (cleanAddress(f.url) === '') return false
  if (c.kind === 'login') return true
  return f.apiKey.trim() !== '' || keepsSecret(c, f)
}

/** The body of the connect request: only the fields of the service's kind. */
export function requestOf(c: MigrateConnect, f: ConnectFields): MigrateConnectRequest {
  const url = f.url.trim()
  return c.kind === 'login' ? { url, username: f.username.trim(), password: f.password } : { url, apiKey: f.apiKey.trim() }
}
