import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import type { Recommendation } from '../../src/api/types'

const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
const fake = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'
const mailpit = process.env.E2E_MAILPIT_URL ?? 'http://127.0.0.1:8025'

type HealthFixture = { catalogDiscovery: { activeDiscoveredGames: number; status: string; reason?: string } }
type CatalogCounters = { fixtureDate: string; search: Record<string, number>; appdetails: Record<string, number>; ai: number; aiByApp: Record<string, number> }
type CatalogRunState = {
  status: 'queued' | 'running' | 'waiting' | 'retryable' | 'succeeded' | 'degraded' | 'dead'
  publishedAppIds: number[]
  processedAppIds: number[]
  committedPages: Array<{ feed: string; start: number; appIds: number[] }>
  feeds: Record<string, { status: string }>
}

const searchCutoverPending = 'Prepared for Task 15: the application does not register the Steam Search scheduler and handlers until the Task 14 production cutover lands.'

async function loadHealth(page: Page): Promise<HealthFixture> {
  const response = await page.request.get(`${api}/api/health`)
  expect(response.ok()).toBeTruthy()
  return response.json() as Promise<HealthFixture>
}

async function loadRecommendations(page: Page): Promise<Recommendation[]> {
  const response = await page.request.get(`${api}/api/recommendations/overview`)
  expect(response.ok()).toBeTruthy()
  const value = await response.json() as { discoveries: { featured: Recommendation | null; items: Recommendation[] }; wishlist: { items: Recommendation[] } }
  return [value.discoveries.featured, ...value.discoveries.items, ...value.wishlist.items].filter((item): item is Recommendation => item !== null)
}

async function resetFakes(request: APIRequestContext) {
  for (const path of ['/__test/reset', '/__test/steam-privacy?mode=public', '/__test/wishlist-mode?mode=public', '/__test/catalog-mode?mode=healthy', '/__test/search-mode?mode=healthy', '/__test/search-reviews?mode=initial']) {
    expect((await request.post(`${fake}${path}`)).status()).toBe(204)
  }
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
async function enqueueManualCatalogRun(page: Page, csrfToken: string): Promise<string> {
  const response = await page.request.post(`${api}/api/test/catalog/runs`, { headers: { 'X-CSRF-Token': csrfToken } })
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

async function waitForCatalogRun(page: Page, runId: string, terminal: CatalogRunState['status']) {
  await expect.poll(async () => (await loadCatalogRun(page, runId)).status, { timeout: 120_000 }).toBe(terminal)
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

test('discovered game survives a later failed page and stays outside wishlist', async ({ page, request }) => {
  test.setTimeout(180_000)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }

  await runSync(page, csrfToken)
  await expect.poll(async () => (await loadHealth(page)).catalogDiscovery.activeDiscoveredGames, { timeout: 90_000 }).toBeGreaterThan(0)

  // Rebuild once the independently discovered candidate is durable. The
  // discovery scheduler itself remains hourly/debounced in production.
  await runSync(page, csrfToken)
  await expect.poll(async () => (await loadRecommendations(page)).some((item) => item.steamAppId === 424242), { timeout: 90_000 }).toBeTruthy()
  const beforeFailure = await loadRecommendations(page)
  const discovered = beforeFailure.find((item) => item.steamAppId === 424242)
  expect(discovered).toBeTruthy()
  expect(discovered?.wishlisted ?? false).toBeFalsy()

  const calendar = await (await page.request.get(`${api}/api/calendar`)).json() as Record<string, Recommendation[]>
  const { fixtureDate } = await loadCatalogCounters(request)
  expect(calendar.exactDates.some((item) => item.steamAppId === 424242 && item.release.exactDate === addUTCDays(fixtureDate, 28))).toBeTruthy()

  await expect.poll(async () => (await loadHealth(page)).catalogDiscovery.status, { timeout: 90_000 }).toBe('degraded')
  const afterFailure = await loadRecommendations(page)
  expect(afterFailure.map((item) => item.id)).toEqual(beforeFailure.map((item) => item.id))

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Fixture Discovered Co-op' }).first()).toBeVisible()
  await expect(page.getByText('Обновление каталога недоступно')).toBeVisible()
  await expect(page.getByText('Последние сохранённые рекомендации остаются видны.')).toBeVisible()
})

test('Steam Search publishes popular results first and applies the released review gates everywhere', async ({ page, request }) => {
  test.skip(true, searchCutoverPending)
  test.setTimeout(240_000)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  await request.delete(`${mailpit}/api/v1/messages`)
  expect((await request.post(`${fake}/__test/search-mode?mode=tail_rate_limited`)).status()).toBe(204)
  const runId = await enqueueManualCatalogRun(page, csrfToken)

  await expect.poll(async () => {
    const run = await loadCatalogRun(page, runId)
    return run.publishedAppIds.includes(520001) && run.feeds.recent?.status !== 'succeeded'
  }, { timeout: 120_000 }).toBeTruthy()
  expect((await request.post(`${fake}/__test/search-mode?mode=healthy`)).status()).toBe(204)
  await waitForCatalogRun(page, runId, 'succeeded')
  await expect.poll(async () => (await loadRecommendations(page)).map((item) => item.steamAppId), { timeout: 120_000 }).toContain(520001)
  const recommendations = await loadRecommendations(page)
  expect(recommendations.map((item) => item.steamAppId)).toContain(520002)
  expect(recommendations.map((item) => item.steamAppId)).not.toContain(520003)
  expect(recommendations.map((item) => item.steamAppId)).not.toContain(520004)
  const positive = recommendations.find((item) => item.steamAppId === 520001)
  if (!positive) throw new Error('positive released fixture must be recommended')
  const detail = await page.request.get(`${api}/api/recommendations/${positive.id}`)
  expect(detail.ok()).toBeTruthy()
  expect((await detail.json() as Recommendation).reviews?.positivePercent).toBe(91)

  expect((await page.request.post(`${api}/api/digests/test`, { headers: { 'X-CSRF-Token': csrfToken } })).status()).toBe(202)
  let messageID = ''
  await expect.poll(async () => {
    const mailbox = await (await request.get(`${mailpit}/api/v1/messages`)).json() as { messages?: Array<{ ID: string }> }
    messageID = mailbox.messages?.[0]?.ID ?? ''
    return messageID
  }, { timeout: 30_000 }).not.toBe('')
  const renderedEmail = JSON.stringify(await (await request.get(`${mailpit}/api/v1/message/${messageID}`)).json())
  expect(renderedEmail).toContain('Fixture Positive Released')
  expect(renderedEmail).not.toContain('Fixture Mixed Released')
  expect(renderedEmail).not.toContain('Fixture Unrated Day Eight')

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Fixture Positive Released' }).first()).toBeVisible()
  await expect(page.getByLabel('Отзывы игроков в Steam: 91% положительных · 1 284 отзыва').first()).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Fixture Mixed Released' })).toHaveCount(0)
  await page.goto('/calendar')
  await expect(page.getByText('Fixture Positive Released').first()).toBeVisible()
  await expect(page.getByText('Fixture Mixed Released')).toHaveCount(0)
})

test('duplicate Search observations do not repeat appdetails or catalog AI work', async ({ page, request }) => {
  test.skip(true, searchCutoverPending)
  test.setTimeout(240_000)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  const firstRun = await enqueueManualCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, firstRun, 'succeeded')
  const first = await loadCatalogCounters(request)

  const replayRun = await enqueueManualCatalogRun(page, csrfToken)
  await waitForCatalogRun(page, replayRun, 'succeeded')
  const replay = await loadCatalogCounters(request)
  expect(replay.appdetails['520001']).toBe(first.appdetails['520001'])
  expect(replay.ai).toBe(first.ai)
})

test('partial popular-feed parser failure preserves last-known-good while interactive sync remains live', async ({ page, request }) => {
  test.skip(true, searchCutoverPending)
  test.setTimeout(240_000)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  await expect.poll(async () => (await loadRecommendations(page)).some((item) => item.steamAppId === 520001), { timeout: 120_000 }).toBeTruthy()
  const beforeFailure = await loadRecommendations(page)

  expect((await request.post(`${fake}/__test/search-mode?mode=popular_parser_broken`)).status()).toBe(204)
  const failedRun = await enqueueManualCatalogRun(page, csrfToken)
  const syncStartedAt = Date.now()
  await runSync(page, csrfToken)
  expect(Date.now() - syncStartedAt).toBeLessThan(90_000)
  await waitForCatalogRun(page, failedRun, 'degraded')
  await expect.poll(async () => (await loadHealth(page)).catalogDiscovery.status, { timeout: 120_000 }).toBe('degraded')
  expect((await loadRecommendations(page)).map((item) => item.id)).toEqual(beforeFailure.map((item) => item.id))
})

test('restart resumes the durable Search run without duplicate metadata work', async ({ page, request }) => {
  test.skip(true, searchCutoverPending)
  test.setTimeout(240_000)
  await resetFakes(request)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const me = await page.request.get(`${api}/api/me`)
  const { csrfToken } = await me.json() as { csrfToken: string }
  expect((await request.post(`${fake}/__test/search-mode?mode=tail_rate_limited`)).status()).toBe(204)
  const interruptedRun = await enqueueManualCatalogRun(page, csrfToken)
  await expect.poll(async () => {
    const run = await loadCatalogRun(page, interruptedRun)
    const committed = run.committedPages.some((item) => item.feed === 'popular_new' && item.appIds.includes(520001))
    return committed && run.processedAppIds.includes(520001) && /waiting|retryable/.test(run.status)
  }, { timeout: 120_000 }).toBeTruthy()
  const beforeRestart = await loadCatalogCounters(request)
  expect(beforeRestart.appdetails['520001']).toBeGreaterThan(0)
  expect(beforeRestart.aiByApp['520001']).toBeGreaterThan(0)

  await restartCatalogRuntime(page, csrfToken)
  expect((await request.post(`${fake}/__test/search-mode?mode=healthy`)).status()).toBe(204)
  await waitForCatalogRun(page, interruptedRun, 'succeeded')
  const afterRestart = await loadCatalogCounters(request)
  expect(afterRestart.appdetails['520001']).toBe(beforeRestart.appdetails['520001'])
  expect(afterRestart.aiByApp['520001']).toBe(beforeRestart.aiByApp['520001'])
})
