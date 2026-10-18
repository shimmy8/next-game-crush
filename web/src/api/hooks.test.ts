import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getCurrentSync, getSyncStatus, preflightSync, startSync } from './client'
import { deriveFreshness, pollSyncJob, useSync } from './hooks'
import type { CalendarResponse, PreferenceProfileData, SyncStatus } from './types'

vi.mock('./client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./client')>()
  return { ...actual, getCurrentSync: vi.fn(), getSyncStatus: vi.fn(), preflightSync: vi.fn(), startSync: vi.fn() }
})

const mockedCurrent = vi.mocked(getCurrentSync)
const mockedPreflight = vi.mocked(preflightSync)
const mockedStart = vi.mocked(startSync)
const mockedStatus = vi.mocked(getSyncStatus)

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

describe('useSync', () => {
  beforeEach(() => {
    mockedCurrent.mockReset()
    mockedPreflight.mockReset()
    mockedStart.mockReset()
    mockedStatus.mockReset()
    mockedCurrent.mockResolvedValue(null)
    mockedStatus.mockResolvedValue(status('succeeded'))
  })

  it('recovers and polls an active sync without repeating preflight or start', async () => {
    mockedCurrent.mockResolvedValue({ ...status('running'), id: 'recovered-job' })
    let finishPoll: ((value: SyncStatus) => void) | undefined
    mockedStatus.mockImplementation(() => new Promise((resolve) => { finishPoll = resolve }))
    const { result } = renderHook(() => useSync('csrf', vi.fn()))

    await waitFor(() => expect(result.current.sync?.id).toBe('recovered-job'))
    expect(mockedStatus).toHaveBeenCalledWith('recovered-job', expect.any(AbortSignal))
    expect(mockedPreflight).not.toHaveBeenCalled()
    expect(mockedStart).not.toHaveBeenCalled()

    await act(async () => { finishPoll?.({ ...status('succeeded'), id: 'recovered-job' }) })
  })

  it('stays quiet when there is no active sync', async () => {
    const { result } = renderHook(() => useSync('csrf', vi.fn()))

    await waitFor(() => expect(mockedCurrent).toHaveBeenCalledTimes(1))
    expect(result.current.sync).toBeNull()
    expect(result.current.requestError).toBe('')
    expect(mockedStatus).not.toHaveBeenCalled()
    expect(mockedPreflight).not.toHaveBeenCalled()
    expect(mockedStart).not.toHaveBeenCalled()
  })

  it('calls completion once when a recovered sync succeeds', async () => {
    mockedCurrent.mockResolvedValue({ ...status('running'), id: 'recovered-job' })
    mockedStatus.mockResolvedValue({ ...status('succeeded'), id: 'recovered-job' })
    const onComplete = vi.fn()
    renderHook(() => useSync('csrf', onComplete))

    await waitFor(() => expect(onComplete).toHaveBeenCalledTimes(1))
    expect(mockedStatus).toHaveBeenCalledTimes(1)
  })

  it('aborts active recovery on unmount', async () => {
    let discoverySignal: AbortSignal | undefined
    mockedCurrent.mockImplementation((signal) => {
      discoverySignal = signal
      return new Promise(() => undefined)
    })
    const { unmount } = renderHook(() => useSync('csrf', vi.fn()))

    await waitFor(() => expect(discoverySignal).toBeDefined())
    unmount()

    expect(discoverySignal?.aborted).toBe(true)
  })

  it('aborts a manually started poll on unmount', async () => {
    let pollingSignal: AbortSignal | undefined
    mockedPreflight.mockResolvedValue({ canSync: true, profileVisibility: 'public', gameDetailsVisibility: 'public', reason: null, displayName: 'Player', avatarUrl: null })
    mockedStart.mockResolvedValue({ jobId: 'manual-job' })
    mockedStatus.mockImplementation((_jobId, signal) => {
      pollingSignal = signal
      return new Promise(() => undefined)
    })
    const { result, unmount } = renderHook(() => useSync('csrf', vi.fn()))
    await waitFor(() => expect(mockedCurrent).toHaveBeenCalledTimes(1))

    await act(async () => { void result.current.run() })
    await waitFor(() => expect(pollingSignal).toBeDefined())
    unmount()

    expect(pollingSignal?.aborted).toBe(true)
  })

  it('cancels stale recovery before a manual preflight and start', async () => {
    let resolveDiscovery: ((value: SyncStatus | null) => void) | undefined
    let discoverySignal: AbortSignal | undefined
    mockedCurrent.mockImplementation((signal) => {
      discoverySignal = signal
      return new Promise((resolve) => { resolveDiscovery = resolve })
    })
    mockedPreflight.mockResolvedValue({ canSync: true, profileVisibility: 'public', gameDetailsVisibility: 'public', reason: null, displayName: 'Player', avatarUrl: null })
    mockedStart.mockResolvedValue({ jobId: 'manual-job' })
    mockedStatus.mockResolvedValue({ ...status('succeeded'), id: 'manual-job' })
    const { result } = renderHook(() => useSync('csrf', vi.fn()))
    await waitFor(() => expect(discoverySignal).toBeDefined())

    await act(async () => { await result.current.run() })
    await act(async () => { resolveDiscovery?.({ ...status('running'), id: 'stale-recovered-job' }) })

    expect(discoverySignal?.aborted).toBe(true)
    expect(mockedPreflight).toHaveBeenCalledTimes(1)
    expect(mockedStart).toHaveBeenCalledTimes(1)
    expect(mockedStatus).toHaveBeenCalledTimes(1)
    expect(mockedStatus).toHaveBeenCalledWith('manual-job', expect.any(AbortSignal))
  })

  it('checks privacy before starting and leaves blocked results retryable', async () => {
    mockedPreflight.mockResolvedValue({
      canSync: false, profileVisibility: 'private', gameDetailsVisibility: 'unknown', reason: 'profile_private', displayName: 'Player', avatarUrl: null,
    })
    const { result } = renderHook(() => useSync('csrf', vi.fn()))

    await act(async () => { await result.current.run() })

    expect(mockedPreflight).toHaveBeenCalledTimes(1)
    expect(mockedStart).not.toHaveBeenCalled()
    expect(result.current.preflight?.reason).toBe('profile_private')
    expect(result.current.checking).toBe(false)
  })

  it('keeps check failures separate and can retry into a started sync', async () => {
    mockedPreflight
      .mockRejectedValueOnce(new Error('502:/api/sync/preflight'))
      .mockResolvedValueOnce({ canSync: true, profileVisibility: 'public', gameDetailsVisibility: 'public', reason: null, displayName: 'Player', avatarUrl: null })
    mockedStart.mockResolvedValue({ jobId: 'job' })
    const onComplete = vi.fn()
    const { result } = renderHook(() => useSync('csrf', onComplete))

    await act(async () => { await result.current.run() })
    expect(result.current.preflightError).toBeTruthy()
    expect(mockedStart).not.toHaveBeenCalled()

    await act(async () => { await result.current.run() })
    await waitFor(() => expect(mockedStart).toHaveBeenCalledTimes(1))
    expect(mockedPreflight.mock.invocationCallOrder[1]).toBeLessThan(mockedStart.mock.invocationCallOrder[0])
    expect(mockedStatus).toHaveBeenCalledWith('job', expect.any(AbortSignal))
    expect(onComplete).toHaveBeenCalledTimes(1)
    expect(result.current.preflightError).toBe('')
    expect(result.current.preflight?.canSync).toBe(true)
  })

  it('does not expose an error for an aborted privacy check', async () => {
    mockedPreflight.mockImplementation(async () => {
      const error = new Error('Aborted')
      error.name = 'AbortError'
      throw error
    })
    const { result } = renderHook(() => useSync('csrf', vi.fn()))

    await act(async () => { await result.current.run() })

    expect(result.current.preflightError).toBe('')
    expect(result.current.requestError).toBe('')
  })
})

describe('deriveFreshness', () => {
  const profile = (createdAt: string): PreferenceProfileData => ({ id: 'p', version: 1, summary: '', summaryStatus: 'unavailable', confidence: .8, evidenceCount: 2, createdAt })

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
