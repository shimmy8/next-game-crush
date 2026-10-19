import type { PreferenceProfileData, Recommendation, RecommendationOverview, ReviewSummary } from '../api/types'

export const reviewFixture: ReviewSummary = {
  band: 'positive', label: 'Very Positive', positivePercent: 91, total: 1284, observedAt: '2026-10-18T03:14:15Z',
}

export function recommendation(id: string, wishlist = false): Recommendation {
  return {
		id, gameId: `game-${id}`, steamAppId: 480, name: id, storeUrl: 'https://store.steampowered.com/app/480/', platforms: undefined, deckCompatibility: 'unknown',
    profileVersion: 1, rankingVersion: 1, algorithmVersion: 'ranking-v2', score: wishlist ? 90 : 20, confidence: .8,
    factors: { wishlist: wishlist ? 1 : 0 }, weights: { wishlist: 0 }, release: { precision: 'unknown' },
		explanation: 'Совпадает с вашими игровыми предпочтениями.', evidence: [], explanationSource: 'none', generatedExplanation: false, latestFeedbackAction: null, latestFeedbackReasons: null,
    createdAt: new Date().toISOString(),
  }
}

export function overviewFixture(discoveries = 0, wishlist = 0): RecommendationOverview {
  const discoveriesItems = Array.from({ length: discoveries }, (_, index) => recommendation(`Discovery ${index + 1}`))
  const wishlistItems = Array.from({ length: wishlist }, (_, index) => recommendation(`Wishlist ${index + 1}`, true))
  return {
    discoveries: { featured: discoveriesItems[0] ?? null, items: discoveriesItems.slice(1, 7), total: discoveries },
    wishlist: { items: wishlistItems.slice(0, 6), total: wishlist, status: 'ready', availability: 'available', lastSuccessfulFetchAt: '2026-10-18T10:00:00Z', degradedReason: null },
    calendar: { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [...discoveriesItems, ...wishlistItems] },
    catalogStatus: { status: 'disabled', activeDiscoveredGames: 0, lastSuccessfulScanAt: null, backfillCompletedAt: null },
  }
}

export function profileFixture(): PreferenceProfileData {
  const publishedAt = new Date().toISOString()
  return { id: 'profile', version: 1, summary: 'Played activity summary.', summaryStatus: 'ready', summarySource: 'deterministic', confidence: .8, evidenceCount: 2, createdAt: publishedAt, publishedAt, updating: false }
}
