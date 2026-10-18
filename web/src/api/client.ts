import type { CurrentUser, PreflightResponse } from './types'

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
  return response.json() as Promise<import('./types').PreferenceProfileData>
}

export function normalizeCalendar(value: Partial<Record<keyof import('./types').CalendarResponse, import('./types').Recommendation[] | null>>): import('./types').CalendarResponse {
  const normalize = (items: import('./types').Recommendation[] | null | undefined) => (items ?? []).map((item) => ({
    ...item,
    factors: item.factors ?? {},
    weights: item.weights ?? {},
    evidence: item.evidence ?? [],
  }))
  return {
    exactDates: normalize(value.exactDates), month: normalize(value.month), quarter: normalize(value.quarter),
    year: normalize(value.year), comingSoon: normalize(value.comingSoon), tba: normalize(value.tba),
  }
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
  return { ...health, jobs }
}

export async function deleteImportedData(csrfToken: string): Promise<void> {
  const response = await fetch('/api/data', { method: 'DELETE', credentials: 'same-origin', headers: { 'X-CSRF-Token': csrfToken } })
  if (!response.ok) throw new Error('Unable to delete imported data')
}
