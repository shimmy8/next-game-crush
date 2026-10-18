import { afterEach, describe, expect, it, vi } from 'vitest'
import { disablePreferenceAdjustment, getDiagnostics, getPreferenceAdjustments, getProfile, getCurrentSync, getRecommendationOverview, normalizeCalendar, normalizeOverview, preflightSync } from './client'
import type { SyncStatus } from './types'
import { overviewFixture, recommendation, reviewFixture } from '../test/overviewFixture'

afterEach(() => vi.unstubAllGlobals())

it('normalizes absent catalog diagnostics from older health responses', async () => {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => new Response(JSON.stringify(url === '/api/jobs' ? [] : { status: 'ok', components: {} }))))
  await expect(getDiagnostics()).resolves.toMatchObject({
    catalogDiscovery: {
      status: 'disabled', activeDiscoveredGames: 0, backfillCursor: 0,
      latestRun: { discovered: 0, evaluated: 0, admitted: 0, skipped: 0, failed: 0 },
      pendingQualifications: 0,
    },
  })
})

describe('preference adjustments', () => {
  it('disables with same-origin credentials, CSRF and encoded ID', async () => {
    const accepted = { adjustmentId: 'rule', rebuildJobId: 'job', previousRankingVersion: 7, alreadyInactive: false }
    const fetch = vi.fn(async () => new Response(JSON.stringify(accepted), { status: 202 }))
    vi.stubGlobal('fetch', fetch)
    const signal = new AbortController().signal
    await expect(disablePreferenceAdjustment('a/b', 'csrf', signal)).resolves.toEqual(accepted)
    expect(fetch).toHaveBeenCalledWith('/api/preference-adjustments/a%2Fb', { credentials: 'same-origin', method: 'DELETE', headers: { 'X-CSRF-Token': 'csrf' }, signal })
  })
  it('reads explicit inactive diagnostics and normalizes old profile arrays', async () => {
    const fetch = vi.fn(async (url: string) => new Response(JSON.stringify(url.startsWith('/api/profile') ? { version: 3, adjustments: null } : null)))
    vi.stubGlobal('fetch', fetch)
    await expect(getPreferenceAdjustments(true)).resolves.toEqual([])
    expect(fetch).toHaveBeenCalledWith('/api/preference-adjustments?includeInactive=true', expect.objectContaining({ credentials: 'same-origin' }))
    await expect(getProfile()).resolves.toMatchObject({ version: 3, adjustments: [] })
  })
  it('rejects failures without exposing response bodies or adjustment IDs', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('private-feedback-canary', { status: 500 })))
    await expect(disablePreferenceAdjustment('private-id', 'csrf')).rejects.toThrow('Unable to update preference adjustment')
    await expect(getPreferenceAdjustments(true)).rejects.toThrow('Unable to load preference adjustments')
  })
})

describe('recommendation overview', () => {
  it('preserves only the normalized review contract in every recommendation collection', () => {
    const raw = { ...recommendation('rated'), reviews: { ...reviewFixture, rawTooltip: '<br>untrusted' } }
    const value = normalizeOverview({
      discoveries: { featured: raw, items: [raw], total: 1 },
      wishlist: { ...overviewFixture().wishlist, items: [raw] },
      calendar: { exactDates: [raw], month: [raw], quarter: [raw], year: [raw], comingSoon: [raw], tba: [raw] },
    })
    for (const item of [value.discoveries.featured!, ...value.discoveries.items, ...value.wishlist.items, ...Object.values(value.calendar).flat()]) {
      expect(item.reviews).toEqual(reviewFixture)
      expect(item.score).toBe(raw.score)
      expect(item.factors).toEqual(raw.factors)
    }
  })

  it('keeps valid unrated, mixed, and last successful review observations', () => {
    const reviews = [
      { ...reviewFixture, observedAt: '2026-10-01T03:14:15Z' },
      { ...reviewFixture, band: 'unrated', label: 'No user reviews', positivePercent: null, total: 0 },
      { ...reviewFixture, band: 'mixed_or_below', label: 'Mixed', positivePercent: 65 },
    ]
    const value = normalizeCalendar(JSON.parse(JSON.stringify({ tba: reviews.map((reviews) => ({ ...recommendation('review'), reviews })) })))
    expect(value.tba.map((item) => item.reviews)).toEqual(reviews)
  })

  it.each([
    null, [], 'Very Positive', {},
    { ...reviewFixture, band: 'unknown' },
    { ...reviewFixture, positivePercent: undefined },
    { ...reviewFixture, positivePercent: null },
    { ...reviewFixture, positivePercent: -1 },
    { ...reviewFixture, positivePercent: 101 },
    { ...reviewFixture, positivePercent: 91.5 },
    { ...reviewFixture, positivePercent: '91' },
    { ...reviewFixture, total: -1 },
    { ...reviewFixture, total: 1.5 },
    { ...reviewFixture, total: Number.MAX_SAFE_INTEGER + 1 },
    { ...reviewFixture, total: 0 },
    { ...reviewFixture, label: '' },
    { ...reviewFixture, label: ' '.repeat(3) },
    { ...reviewFixture, label: 'a'.repeat(65) },
    { ...reviewFixture, label: '<b>Very Positive</b>' },
    { ...reviewFixture, label: 'Very\u0000Positive' },
    { ...reviewFixture, observedAt: undefined },
    { ...reviewFixture, observedAt: 'yesterday' },
    { ...reviewFixture, observedAt: '2026-02-30T03:14:15Z' },
    { ...reviewFixture, band: 'unrated', positivePercent: null },
    { ...reviewFixture, band: 'unrated', total: 0 },
  ].map((reviews) => ({ reviews })))('omits an invalid review object entirely ($reviews)', ({ reviews }) => {
    const raw = JSON.parse(JSON.stringify({ ...recommendation('invalid-review'), reviews }))
    const value = normalizeCalendar({ tba: [raw] }).tba[0]
    expect(value).not.toHaveProperty('reviews')
    expect(value.name).toBe('invalid-review')
  })

  it('requests the overview with credentials and the caller abort signal', async () => {
    const payload = overviewFixture(8, 8)
    const fetch = vi.fn(async () => new Response(JSON.stringify(payload)))
    vi.stubGlobal('fetch', fetch)
    const signal = new AbortController().signal
    await expect(getRecommendationOverview(signal)).resolves.toEqual(payload)
    expect(fetch).toHaveBeenCalledWith('/api/recommendations/overview', { credentials: 'same-origin', signal })
  })

  it('normalizes null metadata and slices honestly without deriving collections from calendar', () => {
    const value = normalizeOverview(JSON.parse('{"discoveries":{"items":null},"wishlist":null,"calendar":{"tba":[]},"catalogStatus":null}'))
    expect(value).toEqual({
      discoveries: { featured: null, items: [], total: 0 },
      wishlist: { items: [], total: 0, status: 'unknown', availability: 'unknown', lastSuccessfulFetchAt: null, degradedReason: null },
      calendar: { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] },
      catalogStatus: { status: 'disabled', activeDiscoveredGames: 0, lastSuccessfulScanAt: null, backfillCompletedAt: null },
    })
    expect(normalizeOverview({ calendar: overviewFixture(8, 8).calendar }).discoveries).toEqual({ featured: null, items: [], total: 0 })
    expect(normalizeOverview({})).toEqual(value)
  })

  it('preserves source freshness, full totals and normalizes every recommendation location identically', () => {
    const payload = overviewFixture(8, 8)
    const raw = { ...recommendation('nullable'), factors: null, weights: null, evidence: null }
    const value = normalizeOverview(JSON.parse(JSON.stringify({
      ...payload,
      discoveries: { ...payload.discoveries, featured: raw, items: [raw] },
      wishlist: { ...payload.wishlist, items: [raw], status: 'stale', availability: 'unavailable', degradedReason: 'private_or_unavailable' },
      calendar: { tba: [raw] },
    })))
    expect(value.discoveries.total).toBe(8)
    expect(value.wishlist).toMatchObject({ total: 8, status: 'stale', availability: 'unavailable', lastSuccessfulFetchAt: payload.wishlist.lastSuccessfulFetchAt, degradedReason: 'private_or_unavailable' })
    for (const item of [value.discoveries.featured, ...value.discoveries.items, ...value.wishlist.items, ...value.calendar.tba]) {
      expect(item).toMatchObject({ factors: {}, weights: {}, evidence: [] })
    }
    expect(normalizeOverview(JSON.parse('{"wishlist":{"status":null,"availability":null,"lastSuccessfulFetchAt":null,"degradedReason":null},"catalogStatus":{"status":null}}')).wishlist.status).toBe('unknown')
  })

  it('preserves completion factor and weight members in the typed recommendation snapshot', () => {
    const raw = { ...recommendation('completion'), factors: { completionAffinity: .75 }, weights: { completionAffinity: 5 } }
    const normalized = normalizeOverview({ discoveries: { featured: raw, items: [], total: 1 } }).discoveries.featured
    expect(normalized?.factors.completionAffinity).toBe(.75)
    expect(normalized?.weights.completionAffinity).toBe(5)
  })

  it('normalizes explanation sources and keeps only allowlisted safe evidence', () => {
    const raw = {
      ...recommendation('grounded'),
      explanationSource: 'ai',
      evidence: [
        { id: 'feature', kind: 'feature_overlap', text: 'Совпадают темы: horror' },
        { id: 'completion', kind: 'completion_anchor', text: 'Есть сходство с пройденной вами игрой SOMA' },
        { id: 'mode', kind: 'negative_mode_preference', text: 'Учтено ваше предпочтение одиночной игры' },
        { id: 'unknown', kind: 'provider_fact', text: 'Секрет провайдера' },
        { id: 'json', kind: 'similar_game', text: '{"steamId":"private"}' },
        { id: 'technical', kind: 'similar_game', text: 'Deterministic score: 90' },
        { id: 'completion-factor', kind: 'completion_anchor', text: 'Поле completionAffinity равно единице' },
        { id: 'completion-weight', kind: 'completion_anchor', text: 'Поле completionAffinityWeight равно пяти' },
        { id: 'secret', kind: 'similar_game', text: '0123456789abcdef0123456789abcdef' },
        { id: 'embedded', kind: 'similar_game', text: 'Причина {"sourceGameIds":["private"]}' },
        { id: '', kind: 'similar_game', text: 'Пустой идентификатор' },
      ],
    }
    const normalized = normalizeOverview({ discoveries: { featured: raw as never, items: [], total: 1 } }).discoveries.featured
    expect(normalized).toMatchObject({
      explanationSource: 'ai',
      evidence: [
        { id: 'feature', kind: 'feature_overlap', text: 'Совпадают темы: horror' },
        { id: 'completion', kind: 'completion_anchor', text: 'Есть сходство с пройденной вами игрой SOMA' },
        { id: 'mode', kind: 'negative_mode_preference', text: 'Учтено ваше предпочтение одиночной игры' },
      ],
    })
  })

  it.each([undefined, 'model', true])('defaults absent or unknown explanation source %s to none', (explanationSource) => {
    const raw = { ...recommendation('legacy'), explanationSource, generatedExplanation: true, explanation: '{"prompt":"unsafe"}' }
    const normalized = normalizeOverview({ discoveries: { featured: raw as never, items: [], total: 1 } }).discoveries.featured
    expect(normalized).toMatchObject({ explanationSource: 'none', evidence: [] })
  })

  it.each([null, {}, 'not-an-array', [null]])('normalizes malformed evidence %j to an empty list', (evidence) => {
    const raw = { ...recommendation('malformed'), explanationSource: 'deterministic', evidence }
    expect(normalizeOverview({ discoveries: { featured: raw as never, items: [], total: 1 } }).discoveries.featured?.evidence).toEqual([])
  })

	it('keeps only nullable direct feedback actions in the recommendation contract', () => {
		const liked = normalizeOverview({ discoveries: { featured: { ...recommendation('liked'), latestFeedbackAction: 'like', latestFeedbackReasons: ['setting', 'genre'] }, items: [], total: 1 } })
		const reminded = normalizeOverview(JSON.parse(JSON.stringify({ discoveries: { featured: { ...recommendation('reminded'), latestFeedbackAction: 'remind_me' }, items: [], total: 1 } })))
		expect(liked.discoveries.featured?.latestFeedbackAction).toBe('like')
		expect(liked.discoveries.featured?.latestFeedbackReasons).toEqual(['setting', 'genre'])
		expect(reminded.discoveries.featured?.latestFeedbackAction).toBeNull()
	})
})

describe('normalizeCalendar', () => {
  it('normalizes Go nil slices without inventing releases', () => {
    expect(normalizeCalendar({ exactDates: null })).toEqual({ exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] })
  })
})

describe('preflightSync', () => {
  it('posts the CSRF-protected preflight request and returns the bounded contract', async () => {
    const payload = {
      canSync: true,
      profileVisibility: 'public',
      gameDetailsVisibility: 'public',
      reason: null,
      displayName: 'Player',
      avatarUrl: 'https://avatars.cloudflare.steamstatic.com/avatar.jpg',
    }
    const fetch = vi.fn(async () => new Response(JSON.stringify(payload), { status: 200 }))
    vi.stubGlobal('fetch', fetch)

    await expect(preflightSync('csrf')).resolves.toEqual(payload)
    expect(fetch).toHaveBeenCalledWith('/api/sync/preflight', expect.objectContaining({
      credentials: 'same-origin',
      method: 'POST',
      headers: { 'X-CSRF-Token': 'csrf' },
    }))
  })
})

describe('getCurrentSync', () => {
  const current: SyncStatus = { id: 'active-job', stage: 'steam', status: 'running', progress: 20, stages: [] }

  it('returns the active sync with same-origin credentials', async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify(current), { status: 200 }))
    vi.stubGlobal('fetch', fetch)

    await expect(getCurrentSync()).resolves.toEqual(current)
    expect(fetch).toHaveBeenCalledWith('/api/sync/current', expect.objectContaining({ credentials: 'same-origin' }))
  })

  it('returns null when there is no active sync', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 204 })))

    await expect(getCurrentSync()).resolves.toBeNull()
  })

  it('rejects other failures without exposing response contents', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('private upstream response', { status: 502 })))

    await expect(getCurrentSync()).rejects.toThrow('Unable to load the current sync')
    await expect(getCurrentSync()).rejects.not.toThrow('private upstream response')
  })
})
