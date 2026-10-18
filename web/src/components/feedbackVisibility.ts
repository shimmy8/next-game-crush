import type { Recommendation, RecommendationOverview } from '../api/types'

const buckets = ['exactDates', 'month', 'quarter', 'year', 'comingSoon', 'tba'] as const

export function findRecommendationByGameId(overview: RecommendationOverview, gameId: string): Recommendation | undefined {
  if (overview.discoveries.featured?.gameId === gameId) return overview.discoveries.featured
  return [...overview.discoveries.items, ...overview.wishlist.items, ...buckets.flatMap((bucket) => overview.calendar[bucket])].find((item) => item.gameId === gameId)
}

export function applyFeedbackTombstones(overview: RecommendationOverview, gameIds: ReadonlySet<string>): RecommendationOverview {
  const visible = (item: Recommendation) => !gameIds.has(item.gameId)
  const discoverySequence = [overview.discoveries.featured, ...overview.discoveries.items].filter((item): item is Recommendation => Boolean(item)).filter(visible)
  const hiddenDiscoveryGames = new Set([overview.discoveries.featured, ...overview.discoveries.items].filter((item): item is Recommendation => item !== null).filter((item) => gameIds.has(item.gameId)).map((item) => item.gameId)).size
  const hiddenWishlistGames = new Set(overview.wishlist.items.filter((item) => gameIds.has(item.gameId)).map((item) => item.gameId)).size
  return {
    discoveries: { featured: discoverySequence[0] ?? null, items: discoverySequence.slice(1), total: Math.max(0, overview.discoveries.total - hiddenDiscoveryGames) },
    wishlist: { ...overview.wishlist, items: overview.wishlist.items.filter(visible), total: Math.max(0, overview.wishlist.total - hiddenWishlistGames) },
    calendar: Object.fromEntries(buckets.map((bucket) => [bucket, overview.calendar[bucket].filter(visible)])) as RecommendationOverview['calendar'],
    catalogStatus: { ...overview.catalogStatus },
  }
}
