export type ReleasePrecision = 'day' | 'month' | 'quarter' | 'year' | 'coming_soon' | 'unknown'

export type ReleaseWindow = {
  precision: ReleasePrecision | string
  exactDate?: string
  raw?: string
}

export type RecommendationFactors = {
  positiveSimilarity?: number
  negativeSimilarity?: number
  wishlist?: number
  tagAffinity?: number
  playtimeAffinity?: number
  achievementAffinity?: number
  recentActivityAffinity?: number
  positiveFeedback?: number
  novelty?: number
  negativeFeedback?: number
  dateUncertainty?: number
  confidence?: number
}

export type Evidence = { id: string; text: string }

export type Recommendation = {
  id: string
  gameId: string
  name: string
  profileVersion: number
  rankingVersion: number
  algorithmVersion: string
  score: number
  confidence: number
  factors: RecommendationFactors
  weights: Record<string, number>
  release: ReleaseWindow
  explanation: string
  evidence: Evidence[]
  generatedExplanation: boolean
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

export type PreferenceProfileData = {
  id: string
  version: number
  summary: string
  confidence: number
  evidenceCount: number
  createdAt: string
}

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

export type DashboardData = { profile: PreferenceProfileData | null; calendar: CalendarResponse }

export type DashboardLoadState =
  | { status: 'loading' }
  | { status: 'ready'; data: DashboardData }
  | { status: 'stale'; data: DashboardData; message: string }
  | { status: 'empty'; data: DashboardData }
  | { status: 'error'; message: string }
