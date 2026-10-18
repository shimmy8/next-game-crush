import { afterEach, describe, expect, it, vi } from 'vitest'
import { getCurrentSync, normalizeCalendar, preflightSync } from './client'
import type { SyncStatus } from './types'

afterEach(() => vi.unstubAllGlobals())

describe('normalizeCalendar', () => {
  it('normalizes Go nil slices without inventing releases', () => {
    expect(normalizeCalendar({ exactDates: null })).toEqual({ exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] })
  })
})

describe('preflightSync', () => {
  it('posts the CSRF-protected preflight request and returns the bounded contract', async () => {
    const payload = {
      canSync: true,
      profileVisibility: 'public',
      gameDetailsVisibility: 'public',
      reason: null,
      displayName: 'Player',
      avatarUrl: 'https://avatars.cloudflare.steamstatic.com/avatar.jpg',
    }
    const fetch = vi.fn(async () => new Response(JSON.stringify(payload), { status: 200 }))
    vi.stubGlobal('fetch', fetch)

    await expect(preflightSync('csrf')).resolves.toEqual(payload)
    expect(fetch).toHaveBeenCalledWith('/api/sync/preflight', expect.objectContaining({
      credentials: 'same-origin',
      method: 'POST',
      headers: { 'X-CSRF-Token': 'csrf' },
    }))
  })
})

describe('getCurrentSync', () => {
  const current: SyncStatus = { id: 'active-job', stage: 'steam', status: 'running', progress: 20, stages: [] }

  it('returns the active sync with same-origin credentials', async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify(current), { status: 200 }))
    vi.stubGlobal('fetch', fetch)

    await expect(getCurrentSync()).resolves.toEqual(current)
    expect(fetch).toHaveBeenCalledWith('/api/sync/current', expect.objectContaining({ credentials: 'same-origin' }))
  })

  it('returns null when there is no active sync', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 204 })))

    await expect(getCurrentSync()).resolves.toBeNull()
  })

  it('rejects other failures without exposing response contents', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('private upstream response', { status: 502 })))

    await expect(getCurrentSync()).rejects.toThrow('Unable to load the current sync')
    await expect(getCurrentSync()).rejects.not.toThrow('private upstream response')
  })
})
