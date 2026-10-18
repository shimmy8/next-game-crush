import { isEvidenceKind } from './types'
import type { CurrentUser, DeckCompatibility, Evidence, ExplanationSource, PlatformSupport, PreflightResponse, Recommendation, RecommendationOverview } from './types'

export type { CurrentUser } from './types'

export async function getCurrentUser(signal?: AbortSignal): Promise<CurrentUser | null> {
  const response = await fetch('/api/me', { credentials: 'same-origin', signal })
  if (response.status === 401) return null
  if (!response.ok) throw new Error('Unable to load the signed-in user')
  return response.json() as Promise<CurrentUser>
}

export async function logout(csrfToken: string): Promise<void> {
  const response = await fetch('/auth/logout', {
    method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': csrfToken },
  })
  if (!response.ok) throw new Error('Unable to sign out')
}

async function requestJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { credentials: 'same-origin', ...init })
  if (!response.ok) throw new Error(`${response.status}:${path}`)
  return response.json() as Promise<T>
}

export async function getProfile(signal?: AbortSignal) {
  const response = await fetch('/api/profile', { credentials: 'same-origin', signal })
  if (response.status === 404) return null
  if (!response.ok) throw new Error(`${response.status}:/api/profile`)
  const value = await response.json() as import('./types').PreferenceProfileData
  return { ...value, adjustments: value.adjustments ?? [] }
}

export async function disablePreferenceAdjustment(id: string, csrfToken: string, signal?: AbortSignal): Promise<import('./types').DisableAdjustmentAccepted> {
  const response = await fetch(`/api/preference-adjustments/${encodeURIComponent(id)}`, {
    credentials: 'same-origin', method: 'DELETE', headers: { 'X-CSRF-Token': csrfToken }, signal,
  })
  if (!response.ok) throw new Error('Unable to update preference adjustment')
  return response.json() as Promise<import('./types').DisableAdjustmentAccepted>
}

export async function getPreferenceAdjustments(includeInactive: boolean, signal?: AbortSignal): Promise<import('./types').PreferenceAdjustment[]> {
  const response = await fetch(`/api/preference-adjustments?includeInactive=${includeInactive}`, { credentials: 'same-origin', signal })
  if (!response.ok) throw new Error('Unable to load preference adjustments')
  return (await response.json() as import('./types').PreferenceAdjustment[] | null) ?? []
}

function normalizePlatforms(value: unknown): PlatformSupport | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined
  if (!('windows' in value) || typeof value.windows !== 'boolean'
    || !('mac' in value) || typeof value.mac !== 'boolean'
    || !('linux' in value) || typeof value.linux !== 'boolean') return undefined
  return { windows: value.windows, mac: value.mac, linux: value.linux }
}

function normalizeDeckCompatibility(value: unknown): DeckCompatibility {
  return value === 'verified' || value === 'playable' || value === 'unsupported' ? value : 'unknown'
}

function normalizeLatestFeedbackAction(value: unknown): import('./types').DirectFeedbackAction | null {
  return value === 'like' || value === 'dislike' ? value : null
}

const feedbackReasons = new Set(['genre', 'setting', 'multiplayer', 'single-player', 'difficulty', 'length', 'early-access', 'already-known', 'not-now', 'other'])
const technicalExplanationText = /(?:deterministic\s+score|coming_soon\s*:|negativeModePreference(?:Weight)?|completionAffinity(?:Weight)?|(?:positiveSimilarity|negativeSimilarity|wishlist|tagAffinity|playtimeAffinity|achievementAffinity|recentActivityAffinity|completionAffinity|positiveFeedback|novelty|negativeFeedback|dateUncertainty|confidence)"?\s*:|api[ _-]?key|authorization|bearer|session|cookie|csrf|prompt)/i
const privateIdentifierText = /(?:\bsk-[a-z0-9_-]{8,}\b|\b7656119\d{10}\b|\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b|\b[0-9a-f]{32,64}\b|\b\d{16,}\b|\b[^\s@]+@[^\s@]+\.[^\s@]+\b)/i
const unsafeEvidenceSyntax = /[{}[\]]/
const evidenceID = /^[a-z0-9][a-z0-9:_-]{0,119}$/

function isJSONObjectText(text: string) {
  if (!text.startsWith('{') && !text.startsWith('[')) return false
  try {
    const value: unknown = JSON.parse(text)
    return typeof value === 'object' && value !== null
  } catch {
    return false
  }
}

function hasUnsafeControlCharacter(text: string) {
  return [...text].some((character) => {
    const code = character.charCodeAt(0)
    return code < 32 && code !== 9 && code !== 10 && code !== 13
  })
}

function safeRecommendationText(value: unknown, maxRunes: number): string {
  if (typeof value !== 'string') return ''
  const text = value.trim()
  return text && [...text].length <= maxRunes && !unsafeEvidenceSyntax.test(text) && !hasUnsafeControlCharacter(text) && !(text.includes('"') && text.includes(':')) && !isJSONObjectText(text) && !technicalExplanationText.test(text) && !privateIdentifierText.test(text) ? text : ''
}

export function safeRecommendationEvidenceText(value: unknown): string {
  return safeRecommendationText(value, 240)
}

export function safeRecommendationExplanationText(value: unknown): string {
  return safeRecommendationText(value, 1000)
}

function normalizeExplanationSource(value: unknown): ExplanationSource {
  return value === 'ai' || value === 'deterministic' ? value : 'none'
}

function normalizeEvidence(value: unknown, source: ExplanationSource): Evidence[] {
  if (source === 'none' || !Array.isArray(value)) return []
  const seen = new Set<string>()
  const seenText = new Set<string>()
  const normalized: Evidence[] = []
  for (const entry of value) {
    if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) continue
    const candidate = entry as Record<string, unknown>
    const text = safeRecommendationEvidenceText(candidate.text)
    if (typeof candidate.id !== 'string' || !evidenceID.test(candidate.id) || seen.has(candidate.id) || !isEvidenceKind(candidate.kind) || !text || seenText.has(text)) continue
    seen.add(candidate.id)
    seenText.add(text)
    normalized.push({ id: candidate.id, kind: candidate.kind, text })
  }
  return normalized
}

function normalizeLatestFeedbackReasons(value: unknown): string[] | null {
  if (value === null || value === undefined) return null
  if (!Array.isArray(value) || value.some((reason) => typeof reason !== 'string' || !feedbackReasons.has(reason))) return null
  return [...new Set(value)]
}

function normalizeRecommendation(item: Recommendation): Recommendation {
  const explanationSource = normalizeExplanationSource(item.explanationSource)
  return {
    ...item,
    platforms: normalizePlatforms(item.platforms),
    deckCompatibility: normalizeDeckCompatibility(item.deckCompatibility),
    latestFeedbackAction: normalizeLatestFeedbackAction(item.latestFeedbackAction),
    latestFeedbackReasons: normalizeLatestFeedbackReasons(item.latestFeedbackReasons),
    ...(typeof item.wishlisted === 'boolean' ? { wishlisted: item.wishlisted } : {}),
    factors: item.factors ?? {},
    weights: item.weights ?? {},
    explanationSource,
    evidence: normalizeEvidence(item.evidence, explanationSource),
  }
}

export function normalizeCalendar(value: Partial<Record<keyof import('./types').CalendarResponse, import('./types').Recommendation[] | null>>): import('./types').CalendarResponse {
  const normalize = (items: Recommendation[] | null | undefined) => (items ?? []).map(normalizeRecommendation)
  return {
    exactDates: normalize(value.exactDates), month: normalize(value.month), quarter: normalize(value.quarter),
    year: normalize(value.year), comingSoon: normalize(value.comingSoon), tba: normalize(value.tba),
  }
}

export function normalizeOverview(value: Partial<RecommendationOverview>): RecommendationOverview {
  const { discoveries, wishlist, catalogStatus } = value
  return {
    discoveries: {
      featured: discoveries?.featured ? normalizeRecommendation(discoveries.featured) : null,
      items: (discoveries?.items ?? []).map(normalizeRecommendation), total: discoveries?.total ?? 0,
    },
    wishlist: {
      items: (wishlist?.items ?? []).map(normalizeRecommendation), total: wishlist?.total ?? 0,
      status: wishlist?.status ?? 'unknown', availability: wishlist?.availability ?? 'unknown',
      lastSuccessfulFetchAt: wishlist?.lastSuccessfulFetchAt ?? null, degradedReason: wishlist?.degradedReason ?? null,
    },
    calendar: normalizeCalendar(value.calendar ?? {}),
    catalogStatus: {
      status: catalogStatus?.status ?? 'disabled', activeDiscoveredGames: catalogStatus?.activeDiscoveredGames ?? 0,
      lastSuccessfulScanAt: catalogStatus?.lastSuccessfulScanAt ?? null, backfillCompletedAt: catalogStatus?.backfillCompletedAt ?? null,
    },
  }
}

export async function getRecommendationOverview(signal?: AbortSignal): Promise<RecommendationOverview> {
  return normalizeOverview(await requestJSON<Partial<RecommendationOverview>>('/api/recommendations/overview', { signal }))
}

export async function getCalendar(signal?: AbortSignal) {
  const value = await requestJSON<Partial<Record<keyof import('./types').CalendarResponse, import('./types').Recommendation[] | null>>>('/api/calendar', { signal })
  return normalizeCalendar(value)
}

export function startSync(csrfToken: string, signal?: AbortSignal) {
  return requestJSON<{ jobId: string }>('/api/sync', {
    method: 'POST', headers: { 'X-CSRF-Token': csrfToken }, signal,
  })
}

export function preflightSync(csrfToken: string, signal?: AbortSignal) {
  return requestJSON<PreflightResponse>('/api/sync/preflight', {
    method: 'POST', headers: { 'X-CSRF-Token': csrfToken }, signal,
  })
}

export function getSyncStatus(jobId: string, signal?: AbortSignal) {
  return requestJSON<import('./types').SyncStatus>(`/api/sync/${encodeURIComponent(jobId)}`, { signal })
}

export async function getCurrentSync(signal?: AbortSignal): Promise<import('./types').SyncStatus | null> {
  const response = await fetch('/api/sync/current', { credentials: 'same-origin', signal })
  if (response.status === 204) return null
  if (!response.ok) throw new Error('Unable to load the current sync')
  return response.json() as Promise<import('./types').SyncStatus>
}

export function submitFeedback(recommendationId: string, csrfToken: string, input: import('./types').FeedbackInput, signal?: AbortSignal) {
  const endpoint = input.action === 'remind_me' ? 'reminder' : 'feedback'
  return requestJSON<import('./types').FeedbackAccepted>(`/api/recommendations/${encodeURIComponent(recommendationId)}/${endpoint}`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken }, body: JSON.stringify(input), signal,
  })
}

export function getRankingDiff(version: number, jobId: string, signal?: AbortSignal) {
  return requestJSON<import('./types').RankingDiff>(`/api/rankings/${version}/diff?jobId=${encodeURIComponent(jobId)}`, { signal })
}

export function sendTestDigest(csrfToken: string, signal?: AbortSignal) {
  return requestJSON<{ deliveryId: string, status: 'pending' }>('/api/digests/test', {
    method: 'POST', headers: { 'X-CSRF-Token': csrfToken }, signal,
  })
}

export async function getDiagnostics(signal?: AbortSignal): Promise<import('./types').DiagnosticsData> {
  const [health, jobs] = await Promise.all([
    requestJSON<Omit<import('./types').DiagnosticsData, 'jobs'>>('/api/health', { signal }),
    requestJSON<import('./types').JobDiagnostic[]>('/api/jobs', { signal }),
  ])
  return {
    ...health,
    catalogDiscovery: health.catalogDiscovery ?? {
      status: 'disabled', activeDiscoveredGames: 0, lastSuccessfulScanAt: null, backfillCompletedAt: null,
      backfillCursor: 0, latestRun: { discovered: 0, evaluated: 0, admitted: 0, skipped: 0, failed: 0 },
      activeBySource: {}, pendingQualifications: 0, reason: 'disabled',
    },
    jobs,
  }
}

export async function deleteImportedData(csrfToken: string): Promise<void> {
  const response = await fetch('/api/data', { method: 'DELETE', credentials: 'same-origin', headers: { 'X-CSRF-Token': csrfToken } })
  if (!response.ok) throw new Error('Unable to delete imported data')
}
