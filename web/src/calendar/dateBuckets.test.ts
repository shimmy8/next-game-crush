import { describe, expect, it } from 'vitest'
import { bucketRelease } from './dateBuckets'

describe('bucketRelease', () => {
  it('keeps exact dates on their real day', () => {
    expect(bucketRelease({ precision: 'day', exactDate: '2026-10-24', raw: '24 Oct, 2026' })).toEqual({
      kind: 'exact',
      date: '2026-10-24',
    })
  })

  it.each([
    ['month', 'month'],
    ['quarter', 'quarter'],
    ['year', 'year'],
    ['coming_soon', 'comingSoon'],
    ['unknown', 'tba'],
  ] as const)('puts %s precision only in its named bucket', (precision, kind) => {
    expect(bucketRelease({ precision, raw: 'Unconfirmed' })).toEqual({ kind })
  })

  it('never fabricates a day for an imprecise window', () => {
    expect(bucketRelease({ precision: 'month', raw: 'October 2026' })).not.toHaveProperty('date')
    expect(bucketRelease({ precision: 'day', raw: 'Malformed date' })).toEqual({ kind: 'tba' })
  })
})
