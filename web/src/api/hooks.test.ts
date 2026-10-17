import { describe, expect, it, vi } from 'vitest'
import { deriveFreshness, pollSyncJob } from './hooks'
import type { CalendarResponse, PreferenceProfileData, SyncStatus } from './types'

const emptyCalendar: CalendarResponse = { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] }
const status = (value: string): SyncStatus => ({ id: 'job', stage: 'steam', status: value, progress: 20, stages: [] })

describe('pollSyncJob', () => {
  it('continues through every incomplete status until terminal success', async () => {
    const values = [status('queued'), status('running'), status('waiting'), status('succeeded')]
    const getStatus = vi.fn(async () => values.shift()!)
    const seen: string[] = []
    const result = await pollSyncJob('job', {
      signal: new AbortController().signal,
      onStatus: (value) => seen.push(value.status),
      getStatus,
      wait: async () => undefined,
    })
    expect(seen).toEqual(['queued', 'running', 'waiting', 'succeeded'])
    expect(result?.status).toBe('succeeded')
  })

  it('cancels an incomplete poll instead of silently completing it', async () => {
    const controller = new AbortController()
    const getStatus = vi.fn(async () => status('running'))
    const result = await pollSyncJob('job', {
      signal: controller.signal,
      onStatus: () => undefined,
      getStatus,
      wait: async () => { controller.abort() },
    })
    expect(result).toBeUndefined()
    expect(getStatus).toHaveBeenCalledTimes(1)
  })

  it('treats backend ready as terminal success', async () => {
    const getStatus = vi.fn(async () => status('ready'))
    const result = await pollSyncJob('job', { signal: new AbortController().signal, onStatus: () => undefined, getStatus, wait: async () => undefined })
    expect(result?.status).toBe('ready')
    expect(getStatus).toHaveBeenCalledTimes(1)
  })
})

describe('deriveFreshness', () => {
  const profile = (createdAt: string): PreferenceProfileData => ({ id: 'p', version: 1, summary: '', confidence: .8, evidenceCount: 2, createdAt })

  it('never treats missing or invalid timestamps as fresh', () => {
    expect(deriveFreshness(null, emptyCalendar, Date.parse('2026-10-17T00:00:00Z')).kind).toBe('unknown')
    expect(deriveFreshness(profile('not-a-date'), emptyCalendar, Date.parse('2026-10-17T00:00:00Z')).kind).toBe('unknown')
  })

  it('requires a valid profile timestamp even when calendar data exists', () => {
    const calendar = { ...emptyCalendar, exactDates: [{ createdAt: '2026-10-16T00:00:00Z' } as never] }
    expect(deriveFreshness(null, calendar, Date.parse('2026-10-17T00:00:00Z')).kind).toBe('unknown')
  })

  it('marks an old valid profile stale and a recent one fresh', () => {
    const now = Date.parse('2026-10-17T00:00:00Z')
    expect(deriveFreshness(profile('2026-10-01T00:00:00Z'), emptyCalendar, now).kind).toBe('stale')
    expect(deriveFreshness(profile('2026-10-16T00:00:00Z'), emptyCalendar, now).kind).toBe('fresh')
  })

  it('uses the oldest displayed component timestamp', () => {
    const now = Date.parse('2026-10-17T00:00:00Z')
    const calendar = { ...emptyCalendar, exactDates: [{ createdAt: '2026-10-16T00:00:00Z' } as never] }
    expect(deriveFreshness(profile('2026-10-01T00:00:00Z'), calendar, now).kind).toBe('stale')
  })
})
