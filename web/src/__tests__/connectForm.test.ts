// The connection form's rules (migrate/connectForm.ts): when «Подключить»
// may be pressed, when a kept key or password may stay blank, and which
// fields go to the server.
import { describe, expect, it } from 'vitest'
import { blockedBy, canSubmit, cleanAddress, keepsSecret, requestOf, type ConnectFields } from '../migrate/connectForm'
import type { MigrateConnect } from '../types'

const empty: ConnectFields = { url: '', apiKey: '', username: '', password: '' }
const key = (over: Partial<MigrateConnect> = {}): MigrateConnect => ({ kind: 'apikey', url: '', hasSecret: false, connected: false, ...over })
const login = (over: Partial<MigrateConnect> = {}): MigrateConnect => ({ kind: 'login', url: '', hasSecret: false, connected: false, ...over })

describe('cleanAddress', () => {
  it('trims and drops trailing slashes, as the server keeps it', () => {
    expect(cleanAddress('  http://sonarr:8989//  ')).toBe('http://sonarr:8989')
  })
})

describe('canSubmit', () => {
  it('needs an address and, for an API-key service, a key', () => {
    expect(canSubmit(key(), empty)).toBe(false)
    expect(canSubmit(key(), { ...empty, url: 'http://sonarr:8989' })).toBe(false)
    expect(canSubmit(key(), { ...empty, apiKey: 'abc' })).toBe(false)
    expect(canSubmit(key(), { ...empty, url: 'http://sonarr:8989', apiKey: ' abc ' })).toBe(true)
    expect(canSubmit(key(), { ...empty, url: 'http://sonarr:8989', apiKey: '   ' })).toBe(false)
  })

  it('takes a blank key while the kept one is for the same address', () => {
    const kept = key({ url: 'http://sonarr:8989', hasSecret: true, connected: true })
    expect(canSubmit(kept, { ...empty, url: 'http://sonarr:8989/' })).toBe(true)
    expect(canSubmit(kept, { ...empty, url: 'http://other:8989' })).toBe(false)
  })

  it('lets qBittorrent in without a password (its web interface may not ask)', () => {
    expect(canSubmit(login(), { ...empty, url: 'http://qbittorrent:8080' })).toBe(true)
    expect(canSubmit(login(), empty)).toBe(false)
  })
})

describe('keepsSecret', () => {
  it('is false while nothing is kept', () => {
    expect(keepsSecret(key({ url: 'http://sonarr:8989' }), { ...empty, url: 'http://sonarr:8989' })).toBe(false)
  })

  it('is true for the kept address, whatever the slash or the spaces', () => {
    const kept = key({ url: 'http://sonarr:8989', hasSecret: true, connected: true })
    expect(keepsSecret(kept, { ...empty, url: ' http://sonarr:8989/ ' })).toBe(true)
    expect(keepsSecret(kept, { ...empty, url: 'http://sonarr:9999' })).toBe(false)
  })

  it('needs qBittorrent\'s login to be the same too', () => {
    const kept = login({ url: 'http://qbittorrent:8080', username: 'admin', hasSecret: true, connected: true })
    expect(keepsSecret(kept, { ...empty, url: 'http://qbittorrent:8080', username: 'admin ' })).toBe(true)
    expect(keepsSecret(kept, { ...empty, url: 'http://qbittorrent:8080', username: 'someone' })).toBe(false)
  })
})

describe('requestOf', () => {
  it('sends the address and the key of an API-key service, nothing else', () => {
    expect(requestOf(key(), { url: ' http://sonarr:8989 ', apiKey: ' abc ', username: 'x', password: 'y' })).toEqual({ url: 'http://sonarr:8989', apiKey: 'abc' })
  })

  it('sends the address, the login and the password of qBittorrent, not a key', () => {
    expect(requestOf(login(), { url: 'http://qbittorrent:8080', apiKey: 'zzz', username: ' admin ', password: ' p ' })).toEqual({
      url: 'http://qbittorrent:8080',
      username: 'admin',
      password: ' p ', // a password is sent as typed
    })
  })

  it('sends a blank key as blank, so the server keeps the kept one', () => {
    expect(requestOf(key({ url: 'http://sonarr:8989', hasSecret: true, connected: true }), { ...empty, url: 'http://sonarr:8989' })).toEqual({
      url: 'http://sonarr:8989',
      apiKey: '',
    })
  })
})

describe('blockedBy', () => {
  it('says what «Подключить» waits for: the address first, then the key', () => {
    expect(blockedBy(key(), empty)).toBe('url')
    expect(blockedBy(key(), { ...empty, url: 'http://sonarr:8989' })).toBe('apiKey')
    expect(blockedBy(key(), { ...empty, url: 'http://sonarr:8989', apiKey: 'abc' })).toBe('')
    // a kept key for the same address is enough
    expect(blockedBy(key({ url: 'http://sonarr:8989', hasSecret: true, connected: true }), { ...empty, url: 'http://sonarr:8989' })).toBe('')
    // qBittorrent needs the address only
    expect(blockedBy(login(), { ...empty, url: 'http://qbittorrent:8080' })).toBe('')
  })
})
