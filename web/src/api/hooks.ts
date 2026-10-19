import { useCallback, useEffect, useRef, useState } from 'react'
import { getPreferenceAdjustments, getRecommendationOverview, getCurrentSync, getProfile, getSyncStatus, normalizeOverview, preflightSync, refreshSyncIfStale, startSync } from './client'
import type { CalendarResponse, CurrentUser, DashboardData, DashboardLoadState, DashboardReloadResult, PreflightResponse, SafeReloadReason, SyncStatus } from './types'

const EMPTY_OVERVIEW = normalizeOverview({})

export type Freshness = { kind: 'fresh' | 'stale' | 'unknown'; message: string }

export function deriveFreshness(profile: DashboardData['profile'], calendar: CalendarResponse, now = Date.now()): Freshness {
  if (!profile) return { kind: 'unknown', message: 'Свежесть неизвестна: профиль ещё не собран' }
  const timestamps = [profile.createdAt, ...Object.values(calendar).flat().map((item) => item.createdAt)]
  const parsed = timestamps.map((value) => Date.parse(value))
  if (parsed.some((value) => !Number.isFinite(value))) return { kind: 'unknown', message: 'Свежесть данных неизвестна' }
  const oldest = Math.min(...parsed)
  if (now - oldest > 7 * 24 * 60 * 60 * 1000) return { kind: 'stale', message: 'Часть показанных данных давно не обновлялась' }
  return { kind: 'fresh', message: 'Данные актуальны' }
}

type PollOptions = {
  signal: AbortSignal
  onStatus: (status: SyncStatus) => void
  getStatus?: typeof getSyncStatus
  wait?: (milliseconds: number, signal: AbortSignal) => Promise<void>
}

function waitFor(milliseconds: number, signal: AbortSignal) {
  return new Promise<void>((resolve) => {
    const timer = window.setTimeout(done, milliseconds)
    function done() { window.clearTimeout(timer); signal.removeEventListener('abort', done); resolve() }
    signal.addEventListener('abort', done, { once: true })
  })
}

function isAbortError(error: unknown) {
  return typeof error === 'object' && error !== null && 'name' in error && error.name === 'AbortError'
}

export async function pollSyncJob(jobId: string, options: PollOptions): Promise<SyncStatus | undefined> {
  const read = options.getStatus ?? getSyncStatus
  const wait = options.wait ?? waitFor
  let delay = 800
  while (!options.signal.aborted) {
    const status = await read(jobId, options.signal)
    if (options.signal.aborted) return undefined
    options.onStatus(status)
    if (['succeeded', 'ready', 'dead', 'failed'].includes(status.status)) return status
    await wait(delay, options.signal)
    delay = Math.min(Math.round(delay * 1.45), 5000)
  }
  return undefined
}

export function useDashboardData(initialState?: DashboardLoadState) {
  const [state, setState] = useState<DashboardLoadState>(initialState ?? { status: 'loading' })
  const stateRef = useRef<DashboardLoadState>(initialState ?? { status: 'loading' })
  const request = useRef(0)
  const abort = useRef<AbortController | null>(null)

  const usableData = useCallback((value: DashboardLoadState) => value.status === 'ready' || value.status === 'stale' || value.status === 'unknown' || value.status === 'empty' ? value.data : undefined, [])
  const publish = useCallback((value: DashboardLoadState) => { stateRef.current = value; setState(value) }, [])
  const nonFresh = useCallback((reason: SafeReloadReason): DashboardReloadResult => {
    const data = usableData(stateRef.current)
    return data ? { kind: 'stale', data, reason } : { kind: 'failed', reason }
  }, [usableData])

  const reload = useCallback(async (): Promise<DashboardReloadResult> => {
    abort.current?.abort()
    const controller = new AbortController()
    abort.current = controller
    const current = ++request.current
    try {
      const [profile, adjustments, overview] = await Promise.all([getProfile(controller.signal), getPreferenceAdjustments(false, controller.signal), getRecommendationOverview(controller.signal)])
      if (current !== request.current) return nonFresh('superseded')
      if (controller.signal.aborted) return nonFresh('aborted')
      const data = { profile, adjustments, overview }
      const count = Object.values(overview.calendar).reduce((total, items) => total + items.length, 0)
      if (!profile && count === 0) publish({ status: 'empty', data })
      else {
        const freshness = deriveFreshness(profile, overview.calendar)
        if (freshness.kind === 'fresh') publish({ status: 'ready', data })
        else publish({ status: freshness.kind, data, message: freshness.message })
      }
      return { kind: 'fresh', data }
    } catch {
      if (current !== request.current) return nonFresh('superseded')
      if (controller.signal.aborted) return nonFresh('aborted')
      const previous = usableData(stateRef.current)
      if (previous) publish({ status: 'stale', data: previous, message: 'Не удалось обновить — показываем последние сохранённые данные' })
      else publish({ status: 'error', message: 'Не удалось загрузить профиль и календарь' })
      return previous ? { kind: 'stale', data: previous, reason: 'request_failed' } : { kind: 'failed', reason: 'request_failed' }
    }
  }, [nonFresh, publish, usableData])

  useEffect(() => {
    const timer = !initialState ? window.setTimeout(() => { void reload() }, 0) : undefined
    return () => {
      if (timer !== undefined) window.clearTimeout(timer)
      abort.current?.abort()
    }
  }, [initialState, reload])

  return { state, reload, emptyData: { profile: null, adjustments: [], overview: EMPTY_OVERVIEW } satisfies DashboardData }
}

export function useEntryRefresh(user: CurrentUser, onSyncComplete: () => void): { status: 'idle' | 'checking' | 'syncing' | 'error'; sync: SyncStatus | null } {
  const [status, setStatus] = useState<'idle' | 'checking' | 'syncing' | 'error'>('idle')
  const [sync, setSync] = useState<SyncStatus | null>(null)
  const completed = useRef(onSyncComplete)
  useEffect(() => { completed.current = onSyncComplete }, [onSyncComplete])

  useEffect(() => {
    if (user.accessMode === 'demo') return
    const controller = new AbortController()
    void (async () => {
      setStatus('checking')
      try {
        const decision = await refreshSyncIfStale(user.csrfToken, controller.signal)
        if (controller.signal.aborted) return
        if ((decision.reason === 'missing' || decision.reason === 'stale' || decision.reason === 'active') && decision.jobId) {
          setStatus('syncing')
          const terminal = await pollSyncJob(decision.jobId, { signal: controller.signal, onStatus: setSync })
          if (controller.signal.aborted) return
          if (terminal?.status === 'succeeded' || terminal?.status === 'ready') {
            completed.current()
            setStatus('idle')
          } else if (terminal) setStatus('error')
          return
        }
        setStatus('idle')
      } catch (error) {
        if (!controller.signal.aborted && !isAbortError(error)) setStatus('error')
      }
    })()
    return () => controller.abort()
  }, [user.accessMode, user.csrfToken])

  return { status, sync }
}

export function useSync(csrfToken: string, onComplete: () => void) {
  const [sync, setSync] = useState<SyncStatus | null>(null)
  const [starting, setStarting] = useState(false)
  const [requestError, setRequestError] = useState('')
  const [preflight, setPreflight] = useState<PreflightResponse | null>(null)
  const [preflightError, setPreflightError] = useState('')
  const [checking, setChecking] = useState(false)
  const abort = useRef<AbortController | null>(null)
  const onCompleteRef = useRef(onComplete)

  useEffect(() => {
    onCompleteRef.current = onComplete
  }, [onComplete])

  const handleCompletion = useCallback((terminal: SyncStatus | undefined) => {
    if (terminal?.status === 'succeeded' || terminal?.status === 'ready') onCompleteRef.current()
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    abort.current = controller
    void (async () => {
      try {
        const current = await getCurrentSync(controller.signal)
        if (controller.signal.aborted || !current) return
        setSync(current)
        const terminal = await pollSyncJob(current.id, { signal: controller.signal, onStatus: setSync })
        handleCompletion(terminal)
      } catch (error) {
        if (controller.signal.aborted || isAbortError(error)) return
        setRequestError('Связь с сервером прервалась')
      }
    })()
    return () => abort.current?.abort()
  }, [handleCompletion])

  const run = useCallback(async () => {
    abort.current?.abort()
    const controller = new AbortController()
    abort.current = controller
    let passedPreflight = false
    setChecking(true); setStarting(false); setRequestError(''); setPreflightError(''); setPreflight(null)
    try {
      const checked = await preflightSync(csrfToken, controller.signal)
      if (controller.signal.aborted) return
      setPreflight(checked)
      setChecking(false)
      if (!checked.canSync) return
      passedPreflight = true
      setStarting(true)
      const { jobId } = await startSync(csrfToken, controller.signal)
      const terminal = await pollSyncJob(jobId, { signal: controller.signal, onStatus: setSync })
      handleCompletion(terminal)
    } catch (error) {
      if (controller.signal.aborted || isAbortError(error)) return
      if (!passedPreflight) setPreflightError('Не удалось проверить доступ к Steam')
      else setRequestError('Связь с сервером прервалась')
    } finally {
      if (!controller.signal.aborted) { setChecking(false); setStarting(false) }
    }
  }, [csrfToken, handleCompletion])

  return { sync, starting, requestError, preflight, preflightError, checking, run }
}
