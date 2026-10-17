import { describe, expect, it } from 'vitest'
import { normalizeCalendar } from './client'

describe('normalizeCalendar', () => {
  it('normalizes Go nil slices without inventing releases', () => {
    expect(normalizeCalendar({ exactDates: null })).toEqual({ exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] })
  })
})
