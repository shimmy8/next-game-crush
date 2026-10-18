import { describe, expect, it } from 'vitest'
import { overviewFixture, recommendation } from '../test/overviewFixture'
import { applyFeedbackTombstones, findRecommendationByGameId } from './feedbackVisibility'

describe('feedback tombstone projection', () => {
  it('removes a stable game from every collection without mutating source metadata', () => {
    const overview = overviewFixture(3, 1)
    const target = overview.discoveries.featured!
    for (const bucket of Object.keys(overview.calendar) as (keyof typeof overview.calendar)[]) overview.calendar[bucket] = [target, recommendation(`${bucket}-other`)]
    overview.wishlist.items.push({ ...target, wishlisted: true }); overview.wishlist.total++
    const projected = applyFeedbackTombstones(overview, new Set([target.gameId]))
    expect(projected.discoveries.featured?.gameId).not.toBe(target.gameId)
    expect(Object.values(projected.calendar).flat().some((item) => item.gameId === target.gameId)).toBe(false)
    expect(projected.wishlist.items.some((item) => item.gameId === target.gameId)).toBe(false)
    expect(projected.discoveries.total).toBe(2); expect(projected.wishlist.total).toBe(1)
    expect(overview.discoveries.featured).toBe(target)
    expect(findRecommendationByGameId(overview, target.gameId)).toBe(target)
  })
})
