export type ReleasePrecision = 'day' | 'month' | 'quarter' | 'year' | 'coming_soon' | 'unknown'

export type ReleaseWindow = {
  precision: ReleasePrecision | string
  exactDate?: string
  raw?: string
}

export type RecommendationFactors = {
  negativeModePreference?: number
  positiveSimilarity?: number
  negativeSimilarity?: number
  wishlist?: number
  tagAffinity?: number
  playtimeAffinity?: number
  achievementAffinity?: number
  recentActivityAffinity?: number
  completionAffinity?: number
  positiveFeedback?: number
  novelty?: number
  negativeFeedback?: number
  dateUncertainty?: number
  confidence?: number
}

export type RecommendationWeights = {
  negativeModePreferenceWeight?: number
  semanticSimilarity?: number
  wishlist?: number
  tagAffinity?: number
  playtimeAffinity?: number
  achievementAffinity?: number
  recentActivityAffinity?: number
  completionAffinity?: number
  positiveFeedback?: number
  novelty?: number
  negativeFeedback?: number
  dateUncertainty?: number
  negativeSimilarityMultiplier?: number
}

export const EVIDENCE_KINDS = ['feature_overlap', 'similar_game', 'playtime_anchor', 'recent_activity_anchor', 'completion_anchor', 'achievement_anchor', 'positive_feedback', 'wishlist_context', 'negative_mode_preference'] as const
export type EvidenceKind = typeof EVIDENCE_KINDS[number]
export function isEvidenceKind(value: unknown): value is EvidenceKind {
  return typeof value === 'string' && (EVIDENCE_KINDS as readonly string[]).includes(value)
}
export type Evidence = { id: string; kind: EvidenceKind; text: string }
export type ExplanationSource = 'ai' | 'deterministic' | 'none'

export type PlatformSupport = { windows: boolean; mac: boolean; linux: boolean }
export type DeckCompatibility = 'verified' | 'playable' | 'unsupported' | 'unknown'
export type DirectFeedbackAction = 'like' | 'dislike'

export type ReviewSummary = {
  band: 'positive' | 'mixed_or_below' | 'unrated'
  label: string
  positivePercent: number | null
  total: number
  observedAt: string
}

export type Recommendation = {
  id: string
  gameId: string
  steamAppId: number
  name: string
  coverImageUrl?: string
  storeUrl: string
  platforms?: PlatformSupport
  deckCompatibility: DeckCompatibility
  profileVersion: number
  rankingVersion: number
  algorithmVersion: string
  score: number
  confidence: number
  wishlisted?: boolean
  reviews?: ReviewSummary
  factors: RecommendationFactors
  weights: RecommendationWeights
  release: ReleaseWindow
  explanation: string
  evidence: Evidence[]
  explanationSource: ExplanationSource
  generatedExplanation: boolean
  latestFeedbackAction: DirectFeedbackAction | null
  latestFeedbackReasons: string[] | null
  createdAt: string
}

export type CalendarResponse = {
  exactDates: Recommendation[]
  month: Recommendation[]
  quarter: Recommendation[]
  year: Recommendation[]
  comingSoon: Recommendation[]
  tba: Recommendation[]
}

export type DiscoveryCollection = { featured: Recommendation | null; items: Recommendation[]; total: number }
export type WishlistCollection = {
  items: Recommendation[]
  total: number
  status: 'ready' | 'stale' | 'unavailable' | 'unknown'
  availability: 'available' | 'unavailable' | 'request_failed' | 'not_modified' | 'unknown'
  lastSuccessfulFetchAt: string | null
  degradedReason: 'private_or_unavailable' | 'request_failed' | null
}
export type CatalogStatus = {
  status: 'ready' | 'backfilling' | 'stale' | 'unavailable' | 'disabled'
  activeDiscoveredGames: number
  lastSuccessfulScanAt: string | null
  backfillCompletedAt: string | null
}
export type RecommendationOverview = {
  discoveries: DiscoveryCollection
  wishlist: WishlistCollection
  calendar: CalendarResponse
  catalogStatus: CatalogStatus
}

export type FeedbackAction = 'like' | 'dislike' | 'remind_me'
export type FeedbackInput = { action: FeedbackAction; reason?: string; reasons?: string[]; freeText?: string; idempotencyKey: string }
export type FeedbackAccepted = { eventId: string; rebuildJobId: string; previousRankingVersion: number }
export type RankingDiff = {
  adjustment?: AdjustmentSummary
  rebuildStatus: string
  previousVersion: number
  currentVersion: number
  added: string[]
  removed: string[]
  reordered: string[]
	classification?: { taxonomy: string; polarity: string; confidence: number; actionable: boolean }
}

export type PreferenceProfileData = {
  adjustments: PreferenceAdjustment[]
  id: string
  version: number
  summary: string
  summaryStatus: 'ready' | 'unavailable'
  confidence: number
  evidenceCount: number
  createdAt: string
}

export type AdjustmentSummary = { id: string; label: string; effect: string; active: boolean }
export type PreferenceAdjustment = AdjustmentSummary & {
  dimension: 'mode'; target: string; confidence: number; createdAt: string
}
export type DisableAdjustmentAccepted = { adjustmentId: string; rebuildJobId: string; previousRankingVersion: number; alreadyInactive: boolean }

export type SyncStage = {
  kind: string
  stage: string
  status: string
  availability?: string
  lastSuccessfulFetchAt?: string
  error?: string
}

export type SyncStatus = {
  id: string
  stage: string
  status: string
  progress: number
  lastSuccessfulFetchAt?: string
  error?: string
  stages: SyncStage[]
}

export type SteamVisibility = 'public' | 'private' | 'unknown'

export type PreflightResponse = {
  canSync: boolean
  profileVisibility: SteamVisibility
  gameDetailsVisibility: SteamVisibility
  reason: 'profile_private' | 'game_details_unavailable' | 'check_failed' | null
  displayName: string | null
  avatarUrl: string | null
}

type CurrentUserBase = {
  authenticated: true
  csrfToken: string
  expiresAt: string
  displayName?: string | null
  avatarUrl?: string | null
}

export type CurrentUser = CurrentUserBase & (
  | { accessMode?: 'owner'; steamId: string }
  | { accessMode: 'demo'; steamId?: never }
)

export type DashboardData = { profile: PreferenceProfileData | null; overview: RecommendationOverview }

export type SafeReloadReason = 'request_failed' | 'superseded' | 'aborted'
export type DashboardReloadResult =
  | { kind: 'fresh'; data: DashboardData }
  | { kind: 'stale'; data: DashboardData; reason: SafeReloadReason }
  | { kind: 'failed'; reason: SafeReloadReason }

export type DashboardLoadState =
  | { status: 'loading' }
  | { status: 'ready'; data: DashboardData }
  | { status: 'stale'; data: DashboardData; message: string }
  | { status: 'unknown'; data: DashboardData; message: string }
  | { status: 'empty'; data: DashboardData }
  | { status: 'error'; message: string }

export type ComponentHealth = { status: string; checkedAt: string; freshAt?: string; message?: string }
export type ExplanationFailureReason = 'disabled' | 'rate_limited' | 'timeout' | 'provider_unavailable' | 'refusal' | 'invalid_output' | 'invalid_citation' | 'unsafe_text' | 'unknown'
export type ExplanationDiagnostic = { aiGenerated: number; deterministic: number; none: number; failureReasons: Partial<Record<ExplanationFailureReason, number>> }
export type JobDiagnostic = { id: string; kind: string; stage: string; status: string; attempts: number; createdAt: string; updatedAt: string; error?: string; explanations?: ExplanationDiagnostic }
export type DiscoveryRunCounts = { discovered: number; evaluated: number; admitted: number; skipped: number; failed: number }
export type DiscoveryDiagnostics = {
  status: 'ready' | 'updating' | 'stale' | 'degraded' | 'disabled'
  activeDiscoveredGames: number
  lastSuccessfulScanAt: string | null
  backfillCompletedAt: string | null
  backfillCursor: number
  latestRun: DiscoveryRunCounts
  activeBySource: Record<string, number>
  pendingQualifications: number
  reason?: string
}
export type SearchRunDiagnostic = { kind: 'scheduled' | 'manual' | 'initial' | 'reconciliation'; status: 'complete' | 'partial' | 'failed'; occurredAt: string; completedAt: string | null; reason?: string }
export type SearchFeedDiagnostic = { status: string; pages: number; discovered: number; deduplicated: number; active: number; boundaryStop: boolean; safetyCap?: string; reason?: string }
export type LaneDiagnostic = { queued: number; running: number; deferred: number; failed: number }
export type BudgetDiagnostic = { used: number; remaining: number; limit: number }
export type ThrottleDiagnostic = { concurrency: number; nextAt: string | null }
export type SearchDiagnostics = {
  status: 'ready' | 'backfilling' | 'stale' | 'unavailable' | 'disabled'
  lastScheduledOccurrenceAt: string | null
  lastCompleteRun: SearchRunDiagnostic | null
  lastPartialRun: SearchRunDiagnostic | null
  lastFailedRun: SearchRunDiagnostic | null
  feeds: Record<'popular_upcoming' | 'popular_new' | 'recent' | 'coming_soon', SearchFeedDiagnostic>
  lanes: { interactive: LaneDiagnostic; catalog: LaneDiagnostic }
  budgets: { store: BudgetDiagnostic & { deferred: number }; ai: BudgetDiagnostic }
  throttles: Record<'store' | 'steam_api' | 'openai', ThrottleDiagnostic>
  admitted: { upcoming: number; released: number }
  rejections: { reviewRejected: number; unratedGrace: number; outsideWindow: number; unavailable: number; failed: number }
  cache: { metadataEntries: number; aiEntries: number }
  due: { count: number; nextAt: string | null }
  rebuild: { dirtyRevision: number; pendingWakeups: number }
  lastReconciliationAt: string | null
  freshAt: string | null
}
export type DiagnosticsData = { status: string; components: Record<string, ComponentHealth>; catalogDiscovery?: DiscoveryDiagnostics; catalogSearch?: SearchDiagnostics; jobs: JobDiagnostic[] }
