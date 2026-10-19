import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import type { Recommendation } from '../../src/api/types'

const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
const fake = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'
const mailpit = process.env.E2E_MAILPIT_URL ?? 'http://127.0.0.1:8025'

type HealthFixture = { catalogSearch: { status: string; reason?: string; lanes: { catalog: { queued: number; running: number; deferred: number } } } }
type CatalogCounters = { fixtureDate: string; search: Record<string, number>; appdetails: Record<string, number>; ai: number; aiByApp: Record<string, number>; appList: number; activeSearch: number; maxActiveSearch: number; anchors: { daySeven: number; restart: number; duplicate: number } }
type RecommendationOverview = {
  discoveries: { featured: Recommendation | null; items: Recommendation[] }
  wishlist: { items: Recommendation[] }
  calendar: Record<string, Recommendation[]>
}
type CatalogRunState = {
  status: 'queued' | 'running' | 'waiting' | 'retryable' | 'succeeded' | 'degraded' | 'dead'
  publishedAppIds: number[]
  processedAppIds: number[]
  committedPages: Array<{ feed: string; start: number; appIds: number[] }>
  feeds: Record<string, { status: string }>
}

async function loadHealth(page: Page): Promise<HealthFixture> {
  const response = await page.request.get(`${api}/api/health`)
  expect(response.ok()).toBeTruthy()
  return response.json() as Promise<HealthFixture>
}

async function loadRecommendations(page: Page): Promise<Recommendation[]> {
  const response = await page.request.get(`${api}/api/recommendations/overview`)
  expect(response.ok()).toBeTruthy()
  const value = await response.json() as RecommendationOverview
  const all = [value.discoveries.featured, ...value.discoveries.items, ...value.wishlist.items, ...Object.values(value.calendar).flat()].filter((item): item is Recommendation => item !== null)
  return [...new Map(all.map((item) => [item.id, item])).values()]
}

async function loadDashboardRecommendations(page: Page): Promise<Recommendation[]> {
  const response = await page.request.get(`${api}/api/recommendations/overview`)
  expect(response.ok()).toBeTruthy()
  const value = await response.json() as RecommendationOverview
  return [value.discoveries.featured, ...value.discoveries.items, ...value.wishlist.items].filter((item): item is Recommendation => item !== null)
}

async function resetFakes(request: APIRequestContext) {
  for (const path of ['/__test/reset', '/__test/steam-privacy?mode=public', '/__test/wishlist-mode?mode=public', '/__test/catalog-mode?mode=healthy', '/__test/search-mode?mode=healthy', '/__test/search-reviews?mode=initial']) {
    expect((await request.post(`${fake}${path}`)).status()).toBe(204)
  }
}

async function waitForAutomaticCatalogIdle(page: Page) {
  await expect.poll(async () => {
    const lane = (await loadHealth(page)).catalogSearch.lanes.catalog
    return lane.queued + lane.running + lane.deferred
  }, { timeout: 180_000 }).toBe(0)
}

async function loadCatalogCounters(request: APIRequestContext): Promise<CatalogCounters> {
  const response = await request.get(`${fake}/__test/catalog-counters`)
  expect(response.ok()).toBeTruthy()
  return response.json() as Promise<CatalogCounters>
}

// Required Task 14 harness-only contract. These endpoints must remain
// fail-closed behind TEST_HARNESS_MODE=1 and normal owner/CSRF checks:
//   POST /api/test/catalog/runs -> 202 { runId }
//   GET  /api/test/catalog/runs/{runId} -> CatalogRunState, including durable
//        committedPages and processedAppIds captured from persisted state
//   POST /api/test/catalog/runtime/restart -> 202 after stopping and starting
//        only the catalog worker/scheduler against the same database.
// Keeping actions here makes every prepared assertion fail before that wiring
// exists instead of accidentally comparing two passive snapshots. All three
// app endpoints use page.request so the fixture login session is preserved;
// the standalone request context is reserved for the fake server and Mailpit.
async function enqueueCatalogRun(page: Page, csrfToken: string, kind: 'manual' | 'reconciliation' = 'manual'): Promise<string> {
  const response = await page.request.post(`${api}/api/test/catalog/runs`, { headers: { 'X-CSRF-Token': csrfToken }, data: { kind } })
  expect(response.status(), 'Task 14 must provide the TEST_HARNESS_MODE-only manual catalog-run endpoint').toBe(202)
  const { runId } = await response.json() as { runId?: string }
  expect(runId).toBeTruthy()
  return runId ?? ''
}

async function loadCatalogRun(page: Page, runId: string): Promise<CatalogRunState> {
  const response = await page.request.get(`${api}/api/test/catalog/runs/${runId}`)
  expect(response.ok(), 'Task 14 must expose harness-only durable catalog-run state').toBeTruthy()
  return response.json() as Promise<CatalogRunState>
}

async function waitForCatalogRun(page: Page, runId: string, terminal: CatalogRunState['status'], timeout = 120_000) {
  await expect.poll(async () => (await loadCatalogRun(page, runId)).status, { timeout }).toBe(terminal)
}

async function restartCatalogRuntime(page: Page, csrfToken: string) {
  const response = await page.request.post(`${api}/api/test/catalog/runtime/restart`, { headers: { 'X-CSRF-Token': csrfToken } })
  expect(response.status(), 'Task 14 must provide the TEST_HARNESS_MODE-only controlled catalog-runtime restart endpoint').toBe(202)
}

function addUTCDays(date: string, days: number): string {
  const value = new Date(`${date}T00:00:00Z`)
  value.setUTCDate(value.getUTCDate() + days)
  return value.toISOString().slice(0, 10)
}

async function runSync(page: Page, csrfToken: string) {
  const preflight = await page.request.post(`${api}/api/sync/preflight`, { headers: { 'X-CSRF-Token': csrfToken } })
  expect(preflight.ok()).toBeTruthy()
  expect((await preflight.json() as { canSync: boolean }).canSync).toBeTruthy()
  const response = await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': csrfToken } })
  expect(response.status()).toBe(202)
  const { jobId } = await response.json() as { jobId: string }
  await expect.poll(async () => {
    const status = await page.request.get(`${api}/api/sync/${jobId}`)
    return (await status.json() as { status: string }).status
  }, { timeout: 90_000 }).toBe('succeeded')
}

async function likeRecommendation(page: Page, csrfToken: string, recommendation: Recommendation, suffix: string) {
  const response = await page.request.post(`${api}/api/recommendations/${recommendation.id}/feedback`, {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken },
    data: { action: 'like', reason: 'genre', idempotencyKey: `catalog-e2e-${suffix}` },
  })
  expect(response.status()).toBe(202)
  const accepted = await response.json() as { rebuildJobId: string; previousRankingVersion: number }
  await expect.poll(async () => {
    const status = await page.request.get(`${api}/api/rankings/${accepted.previousRankingVersion}/diff?jobId=${accepted.rebuildJobId}`)
    return (await status.json() as { rebuildStatus: string }).rebuildStatus
  }, { timeout: 120_000 }).toBe('succeeded')
}

async function sendDigestAndLoadRendered(page: Page, request: APIRequestContext, csrfToken: string): Promise<string> {
  const before = await (await request.get(`${mailpit}/api/v1/messages`)).json() as { messages?: Array<{ ID: string }> }
  const previousIDs = new Set((before.messages ?? []).map((message) => message.ID))
  const send = await page.request.post(`${api}/api/digests/test`, { headers: { 'X-CSRF-Token': csrfToken } })
  expect(send.status()).toBe(202)
  let messageID = ''
  await expect.poll(async () => {
    const mailbox = await (await request.get(`${mailpit}/api/v1/messages`)).json() as { messages?: Array<{ ID: string }> }
    messageID = mailbox.messages?.find((message) => !previousIDs.has(message.ID))?.ID ?? ''
    return messageID
  }, { timeout: 30_000 }).not.toBe('')
  return JSON.stringify(await (await request.get(`${mailpit}/api/v1/message/${messageID}`)).json())
}

test('Steam Search publishes popular results first and applies the released review gates everywhere', async ({ page, request }) => {
  test.setTimeout(240_000)
  await waitForAutomaticCatalogIdle(page)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  const fixture = await loadCatalogCounters(request)
  const daySevenAppID = fixture.anchors.daySeven
  expect((await request.post(`${fake}/__test/wishlist-mode?mode=empty`)).status()).toBe(204)
  await runSync(page, csrfToken)
  await request.delete(`${mailpit}/api/v1/messages`)
  expect((await request.post(`${fake}/__test/search-mode?mode=tail_rate_limited`)).status()).toBe(204)
  const runId = await enqueueCatalogRun(page, csrfToken)

  await expect.poll(async () => {
    const run = await loadCatalogRun(page, runId)
    return run.publishedAppIds.includes(520001) && run.feeds.recent?.status !== 'complete'
  }, { timeout: 120_000 }).toBeTruthy()
  await runSync(page, csrfToken)
  const early = await loadRecommendations(page)
  const earlyPositive = early.find((item) => item.steamAppId === 520001)
  if (!earlyPositive) throw new Error('popular feed must publish its released anchor before the tails finish')
  await likeRecommendation(page, csrfToken, earlyPositive, `positive-${runId}`)
  await expect.poll(async () => (await loadDashboardRecommendations(page)).map((item) => item.steamAppId), { timeout: 120_000 }).toContain(520001)
  expect((await loadCatalogRun(page, runId)).status).not.toBe('succeeded')
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Fixture Positive Released' })).toBeVisible()
  expect((await request.post(`${fake}/__test/search-mode?mode=healthy`)).status()).toBe(204)
  await waitForCatalogRun(page, runId, 'succeeded', 240_000)
  await runSync(page, csrfToken)
  await expect.poll(async () => (await loadRecommendations(page)).map((item) => item.steamAppId), { timeout: 120_000 }).toContain(520001)
  let recommendations = await loadRecommendations(page)
  expect(recommendations.map((item) => item.steamAppId)).toContain(daySevenAppID)
  expect(recommendations.map((item) => item.steamAppId)).not.toContain(520003)
  expect(recommendations.map((item) => item.steamAppId)).not.toContain(520004)
  const positive = recommendations.find((item) => item.steamAppId === 520001)
  const unrated = recommendations.find((item) => item.steamAppId === daySevenAppID)
  if (!positive) throw new Error('positive released fixture must be recommended')
  if (!unrated) throw new Error('day-seven unrated fixture must be recommended')
  await likeRecommendation(page, csrfToken, unrated, `unrated-${runId}`)
  recommendations = await loadRecommendations(page)
  const currentPositive = recommendations.find((item) => item.steamAppId === 520001)
  const currentUnrated = recommendations.find((item) => item.steamAppId === daySevenAppID)
  if (!currentPositive || !currentUnrated) throw new Error('released anchors disappeared after dashboard publication')
  const detail = await page.request.get(`${api}/api/recommendations/${currentPositive.id}`)
  expect(detail.ok()).toBeTruthy()
  expect((await detail.json() as Recommendation).reviews?.positivePercent).toBe(91)
  const unratedDetail = await page.request.get(`${api}/api/recommendations/${currentUnrated.id}`)
  expect(unratedDetail.ok()).toBeTruthy()
  expect((await unratedDetail.json() as Recommendation).reviews?.band).toBe('unrated')

  const dashboard = await loadDashboardRecommendations(page)
  expect(dashboard.length).toBeGreaterThan(0)
  expect(dashboard.map((item) => item.steamAppId)).toEqual(expect.arrayContaining([520001, daySevenAppID]))
  expect(dashboard.some((item) => item.steamAppId === 520003 || item.steamAppId === 520004)).toBeFalsy()

  const calendar = await (await page.request.get(`${api}/api/calendar`)).json() as Record<string, Recommendation[]>
  const { fixtureDate } = fixture
  expect(calendar.exactDates.some((item) => item.steamAppId === 520001 && item.release.exactDate === addUTCDays(fixtureDate, -6))).toBeTruthy()
  expect(calendar.exactDates.some((item) => item.steamAppId === daySevenAppID && item.release.exactDate === addUTCDays(fixtureDate, -7))).toBeTruthy()
  expect(calendar.exactDates.some((item) => item.steamAppId === 520003 || item.steamAppId === 520004)).toBeFalsy()

  const renderedEmail = await sendDigestAndLoadRendered(page, request, csrfToken)
  expect(renderedEmail).toContain('Fixture Positive Released')
  expect(renderedEmail).toContain('Fixture Unrated Day Seven')
  expect(renderedEmail).toContain('91%')
  expect(renderedEmail).toContain('Пока нет отзывов')
  expect(renderedEmail).not.toContain('Fixture Mixed Released')
  expect(renderedEmail).not.toContain('Fixture Unrated Day Eight')

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Fixture Positive Released' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Fixture Unrated Day Seven' })).toBeVisible()
  await expect(page.getByText(/91% положительных/).first()).toBeVisible()
  await expect(page.getByText('Пока нет отзывов').first()).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Fixture Mixed Released' })).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'Fixture Unrated Day Eight' })).toHaveCount(0)
  await page.goto('/calendar')
  const positiveEvent = page.locator('.calendar-event').filter({ hasText: 'Fixture Positive Released' }).first()
  await expect(positiveEvent).toBeVisible()
  await positiveEvent.locator('summary').click()
  await expect(positiveEvent.locator('.review-summary')).toBeVisible()
  const unratedEvent = page.locator('.calendar-event').filter({ hasText: 'Fixture Unrated Day Seven' }).first()
  await expect(unratedEvent).toBeVisible()
  await unratedEvent.locator('summary').click()
  await expect(unratedEvent.getByText('Пока нет отзывов')).toBeVisible()
  await expect(page.getByText('Fixture Mixed Released')).toHaveCount(0)
  await expect(page.getByText('Fixture Unrated Day Eight')).toHaveCount(0)

  expect((await request.post(`${fake}/__test/search-reviews?mode=changed`)).status()).toBe(204)
  const mixedRun = await enqueueCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, mixedRun, 'succeeded')
  await runSync(page, csrfToken)
  await expect.poll(async () => (await loadRecommendations(page)).some((item) => item.steamAppId === 520001), { timeout: 120_000 }).toBeFalsy()
  expect((await page.request.get(`${api}/api/recommendations/${currentPositive.id}`)).status()).toBe(404)

  expect((await request.post(`${fake}/__test/search-reviews?mode=initial`)).status()).toBe(204)
  const restoredRun = await enqueueCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, restoredRun, 'succeeded')
  await runSync(page, csrfToken)
  await expect.poll(async () => (await loadDashboardRecommendations(page)).some((item) => item.steamAppId === 520001), { timeout: 120_000 }).toBeTruthy()

  expect((await request.post(`${fake}/__test/search-reviews?mode=expired`)).status()).toBe(204)
  const expiredRun = await enqueueCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, expiredRun, 'succeeded')
  await runSync(page, csrfToken)
  const afterExpiry = await loadDashboardRecommendations(page)
  expect(afterExpiry.length).toBeGreaterThan(0)
  expect(afterExpiry.some((item) => item.steamAppId === 520001)).toBeTruthy()
  expect(afterExpiry.some((item) => item.steamAppId === daySevenAppID)).toBeFalsy()
  expect((await page.request.get(`${api}/api/recommendations/${currentUnrated.id}`)).status()).toBe(404)
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Fixture Positive Released' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Fixture Unrated Day Seven' })).toHaveCount(0)
  await page.goto('/calendar')
  await expect(page.getByRole('heading', { name: 'Fixture Positive Released' })).toBeVisible()
  await expect(page.getByText('Fixture Unrated Day Seven')).toHaveCount(0)
  await request.delete(`${mailpit}/api/v1/messages`)
  const freshDigestAnchor = afterExpiry.find((item) => !renderedEmail.includes(item.name))
  if (!freshDigestAnchor) throw new Error('expiry transition needs a materially new eligible digest anchor')
  const refreshedEmail = await sendDigestAndLoadRendered(page, request, csrfToken)
  expect(refreshedEmail).toContain(freshDigestAnchor.name)
  expect(refreshedEmail).not.toContain('Fixture Unrated Day Seven')
})

test('duplicate Search observations do not repeat appdetails or catalog AI work', async ({ page, request }) => {
  test.setTimeout(240_000)
  await waitForAutomaticCatalogIdle(page)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  const duplicateAppID = (await loadCatalogCounters(request)).anchors.duplicate
  expect((await request.post(`${fake}/__test/search-mode?mode=duplicate_candidate`)).status()).toBe(204)
  const firstRun = await enqueueCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, firstRun, 'succeeded')
  const first = await loadCatalogCounters(request)
  expect(first.appdetails[String(duplicateAppID)]).toBeGreaterThan(0)
  expect(first.aiByApp[String(duplicateAppID)]).toBeGreaterThan(0)

  const replayRun = await enqueueCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, replayRun, 'succeeded')
  const replay = await loadCatalogCounters(request)
  expect(Object.values(replay.search).reduce((total, count) => total + count, 0)).toBeGreaterThan(Object.values(first.search).reduce((total, count) => total + count, 0))
  expect(replay.appdetails[String(duplicateAppID)]).toBe(first.appdetails[String(duplicateAppID)])
  expect(replay.aiByApp[String(duplicateAppID)]).toBe(first.aiByApp[String(duplicateAppID)])
})

test('interactive sync stays live under a real catalog backlog and parser failure preserves last-known-good', async ({ page, request }) => {
  test.setTimeout(300_000)
  await waitForAutomaticCatalogIdle(page)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  expect((await request.post(`${fake}/__test/search-mode?mode=hold_popular`)).status()).toBe(204)
  const backlogRuns: string[] = []
  for (let index = 0; index < 4; index++) {
    backlogRuns.push(await enqueueCatalogRun(page, csrfToken))
    if (index < 3) await page.waitForTimeout(1_050)
  }
  await expect.poll(async () => {
    const health = await loadHealth(page)
    const counters = await loadCatalogCounters(request)
    return { running: health.catalogSearch.lanes.catalog.running, queued: health.catalogSearch.lanes.catalog.queued, active: counters.activeSearch, max: counters.maxActiveSearch }
  }, { timeout: 60_000 }).toMatchObject({ running: 1, active: 1, max: 1 })
  const backlogLane = (await loadHealth(page)).catalogSearch.lanes.catalog
  expect(backlogLane.queued).toBeGreaterThan(0)
  const backlogSyncStartedAt = Date.now()
  await runSync(page, csrfToken)
  expect(Date.now() - backlogSyncStartedAt).toBeLessThan(90_000)
  expect((await loadCatalogCounters(request)).activeSearch).toBe(1)
  expect((await request.post(`${fake}/__test/search-mode?mode=healthy`)).status()).toBe(204)
  for (const backlogRun of backlogRuns) await waitForCatalogRun(page, backlogRun, 'succeeded')

  const baselineRun = await enqueueCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, baselineRun, 'succeeded')
  await runSync(page, csrfToken)
  await expect.poll(async () => (await loadRecommendations(page)).some((item) => item.steamAppId === 520001), { timeout: 120_000 }).toBeTruthy()
  const beforeFailure = await loadRecommendations(page)

  expect((await request.post(`${fake}/__test/search-mode?mode=popular_parser_broken`)).status()).toBe(204)
  const failedRun = await enqueueCatalogRun(page, csrfToken)
  await expect.poll(async () => {
    const run = await loadCatalogRun(page, failedRun)
    const counters = await loadCatalogCounters(request)
    return /queued|running|waiting|retryable/.test(run.status) && (counters.search.popularcomingsoon ?? 0) >= 2
  }, { timeout: 30_000 }).toBeTruthy()
  await runSync(page, csrfToken)
  await waitForCatalogRun(page, failedRun, 'dead')
  await expect.poll(async () => (await loadHealth(page)).catalogSearch.status, { timeout: 180_000 }).toBe('stale')
  const afterFailure = await loadRecommendations(page)
  expect(afterFailure.map((item) => item.steamAppId).sort((a, b) => a - b)).toEqual(beforeFailure.map((item) => item.steamAppId).sort((a, b) => a - b))
  expect(afterFailure.find((item) => item.steamAppId === 520001)?.reviews?.positivePercent).toBe(91)
})

test('restart resumes the durable Search run without duplicate metadata work', async ({ page, request }) => {
  test.setTimeout(240_000)
  await waitForAutomaticCatalogIdle(page)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  const restartAppID = (await loadCatalogCounters(request)).anchors.restart
  expect((await request.post(`${fake}/__test/search-mode?mode=restart_tail_rate_limited`)).status()).toBe(204)
  const interruptedRun = await enqueueCatalogRun(page, csrfToken)
  await expect.poll(async () => {
    const run = await loadCatalogRun(page, interruptedRun)
    const counters = await loadCatalogCounters(request)
    const committed = run.committedPages.some((item) => item.feed === 'popular_new' && item.appIds.includes(restartAppID))
    const tailsStarted = (counters.search.recent ?? 0) + (counters.search.comingsoon ?? 0) > 0
    return committed && (counters.appdetails[String(restartAppID)] ?? 0) > 0 && (counters.aiByApp[String(restartAppID)] ?? 0) > 0 && tailsStarted && /queued|running|waiting|retryable/.test(run.status)
  }, { timeout: 120_000 }).toBeTruthy()
  const durableBeforeRestart = await loadCatalogRun(page, interruptedRun)
  const beforeRestart = await loadCatalogCounters(request)
  const popularPagesBeforeRestart = durableBeforeRestart.committedPages.filter((item) => item.feed === 'popular_new')
  const tailRequestsBeforeRestart = (beforeRestart.search.recent ?? 0) + (beforeRestart.search.comingsoon ?? 0)
  expect(beforeRestart.search.popularnew).toBeGreaterThan(0)
  expect(tailRequestsBeforeRestart).toBeGreaterThan(0)
  expect(beforeRestart.appdetails[String(restartAppID)]).toBeGreaterThan(0)
  expect(beforeRestart.aiByApp[String(restartAppID)]).toBeGreaterThan(0)

  await restartCatalogRuntime(page, csrfToken)
  expect((await request.post(`${fake}/__test/search-mode?mode=healthy`)).status()).toBe(204)
  await waitForCatalogRun(page, interruptedRun, 'succeeded')
  const durableAfterRestart = await loadCatalogRun(page, interruptedRun)
  const afterRestart = await loadCatalogCounters(request)
  expect(afterRestart.search.popularnew).toBe(beforeRestart.search.popularnew)
  expect((afterRestart.search.recent ?? 0) + (afterRestart.search.comingsoon ?? 0)).toBeGreaterThan(tailRequestsBeforeRestart)
  expect(durableAfterRestart.committedPages.filter((item) => item.feed === 'popular_new')).toEqual(popularPagesBeforeRestart)
  expect(durableAfterRestart.processedAppIds).toContain(restartAppID)
  for (const pageBeforeRestart of durableBeforeRestart.committedPages) {
    expect(durableAfterRestart.committedPages).toContainEqual(pageBeforeRestart)
  }
  const pageKeys = durableAfterRestart.committedPages.map((item) => `${item.feed}:${item.start}`)
  expect(new Set(pageKeys).size).toBe(pageKeys.length)
  expect(afterRestart.appdetails[String(restartAppID)]).toBe(beforeRestart.appdetails[String(restartAppID)])
  expect(afterRestart.aiByApp[String(restartAppID)]).toBe(beforeRestart.aiByApp[String(restartAppID)])
})

test('weekly reconciliation uses the bounded App List path independently of Search', async ({ page, request }) => {
  test.setTimeout(300_000)
  await waitForAutomaticCatalogIdle(page)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }

  const runId = await enqueueCatalogRun(page, csrfToken, 'reconciliation')
  await expect.poll(async () => (await loadCatalogCounters(request)).appList, { timeout: 30_000 }).toBe(2)
  await waitForCatalogRun(page, runId, 'succeeded', 240_000)
  const run = await loadCatalogRun(page, runId)
  expect(run.status).toBe('succeeded')
  expect(run.committedPages.some((item) => item.feed === 'reconciliation' && item.appIds.includes(424242))).toBeTruthy()
  expect(run.processedAppIds).toContain(424242)
  expect(run.publishedAppIds).toContain(424242)
  const counters = await loadCatalogCounters(request)
  expect(counters.appList).toBe(2)
  expect(Object.values(counters.search).reduce((total, count) => total + count, 0)).toBe(0)
})
