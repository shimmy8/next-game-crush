import { useCallback, useEffect, useRef, useState } from 'react'
import { getCalendar, getCurrentSync, getProfile, getSyncStatus, preflightSync, startSync } from './client'
import type { DashboardData, DashboardLoadState, PreflightResponse, SyncStatus } from './types'

const EMPTY_CALENDAR: DashboardData['calendar'] = { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] }

export type Freshness = { kind: 'fresh' | 'stale' | 'unknown'; message: string }

export function deriveFreshness(profile: DashboardData['profile'], calendar: DashboardData['calendar'], now = Date.now()): Freshness {
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
  const request = useRef(0)
  const abort = useRef<AbortController | null>(null)

  const reload = useCallback(async () => {
    abort.current?.abort()
    const controller = new AbortController()
    abort.current = controller
    const current = ++request.current
    try {
      const [profile, calendar] = await Promise.all([getProfile(controller.signal), getCalendar(controller.signal)])
      if (current !== request.current || controller.signal.aborted) return
      const data = { profile, calendar }
      const count = Object.values(calendar).reduce((total, items) => total + items.length, 0)
      if (!profile && count === 0) setState({ status: 'empty', data })
      else {
        const freshness = deriveFreshness(profile, calendar)
        if (freshness.kind === 'fresh') setState({ status: 'ready', data })
        else setState({ status: freshness.kind, data, message: freshness.message })
      }
    } catch {
      if (current === request.current && !controller.signal.aborted) setState((previous) => {
        if (previous.status === 'ready' || previous.status === 'stale' || previous.status === 'unknown' || previous.status === 'empty') {
          return { status: 'stale', data: previous.data, message: 'Не удалось обновить — показываем последние сохранённые данные' }
        }
        return { status: 'error', message: 'Не удалось загрузить профиль и календарь' }
      })
    }
  }, [])

  useEffect(() => {
    const timer = !initialState ? window.setTimeout(() => { void reload() }, 0) : undefined
    return () => {
      if (timer !== undefined) window.clearTimeout(timer)
      abort.current?.abort()
    }
  }, [initialState, reload])

  return { state, reload, emptyData: { profile: null, calendar: EMPTY_CALENDAR } as DashboardData }
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
