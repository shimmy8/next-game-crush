import type { ReleaseWindow } from '../api/types'

export type CalendarBucket =
  | { kind: 'exact'; date: string }
  | { kind: 'month' | 'quarter' | 'year' | 'comingSoon' | 'tba' }

export function bucketRelease(window: ReleaseWindow): CalendarBucket {
  if (window.precision === 'day' && window.exactDate && /^\d{4}-\d{2}-\d{2}$/.test(window.exactDate)) {
    const [year, month, day] = window.exactDate.split('-').map(Number)
    const date = new Date(Date.UTC(year, month - 1, day))
    if (date.getUTCFullYear() === year && date.getUTCMonth() === month - 1 && date.getUTCDate() === day) {
      return { kind: 'exact', date: window.exactDate }
    }
  }
  if (window.precision === 'month') return { kind: 'month' }
  if (window.precision === 'quarter') return { kind: 'quarter' }
  if (window.precision === 'year') return { kind: 'year' }
  if (window.precision === 'coming_soon') return { kind: 'comingSoon' }
  return { kind: 'tba' }
}
