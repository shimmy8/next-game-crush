import { useCallback, useEffect, useRef, useState } from 'react'
import { getCalendar, getProfile, getSyncStatus, startSync } from './client'
import type { DashboardData, DashboardLoadState, SyncStatus } from './types'

const EMPTY_CALENDAR: DashboardData['calendar'] = { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] }

export function useDashboardData(initialState?: DashboardLoadState) {
  const [state, setState] = useState<DashboardLoadState>(initialState ?? { status: 'loading' })
  const request = useRef(0)

  const reload = useCallback(async () => {
    const current = ++request.current
    try {
      const [profile, calendar] = await Promise.all([getProfile(), getCalendar()])
      if (current !== request.current) return
      const data = { profile, calendar }
      const count = Object.values(calendar).reduce((total, items) => total + items.length, 0)
      if (!profile && count === 0) setState({ status: 'empty', data })
      else {
        const lastBuilt = profile?.createdAt ? new Date(profile.createdAt).getTime() : Date.now()
        const stale = Date.now() - lastBuilt > 7 * 24 * 60 * 60 * 1000
        setState(stale ? { status: 'stale', data, message: 'Профиль давно не обновлялся' } : { status: 'ready', data })
      }
    } catch {
      if (current === request.current) setState((previous) => {
        if (previous.status === 'ready' || previous.status === 'stale' || previous.status === 'empty') {
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
    }
  }, [initialState, reload])

  return { state, reload, emptyData: { profile: null, calendar: EMPTY_CALENDAR } as DashboardData }
}

export function useSync(csrfToken: string, onComplete: () => void) {
  const [sync, setSync] = useState<SyncStatus | null>(null)
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')

  const run = useCallback(async () => {
    setStarting(true); setError('')
    try {
      const { jobId } = await startSync(csrfToken)
      let delay = 800
      for (let attempt = 0; attempt < 12; attempt++) {
        const status = await getSyncStatus(jobId)
        setSync(status)
        if (status.status === 'succeeded') { onComplete(); break }
        if (status.status === 'dead') { setError(status.error || 'Синхронизация завершилась с ошибкой'); break }
        await new Promise((resolve) => window.setTimeout(resolve, delay))
        delay = Math.min(Math.round(delay * 1.45), 5000)
      }
    } catch { setError('Не удалось запустить синхронизацию') }
    finally { setStarting(false) }
  }, [csrfToken, onComplete])

  return { sync, starting, error, run }
}
