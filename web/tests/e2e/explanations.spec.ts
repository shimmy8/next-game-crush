import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import type { ExplanationFailureReason, Recommendation, RecommendationOverview } from '../../src/api/types'
import { overviewFixture, profileFixture, recommendation } from '../../src/test/overviewFixture'

const api = 'http://127.0.0.1:18080'
const fake = 'http://127.0.0.1:18090'
const openAIDisabled = process.env.E2E_OPENAI_DISABLED === '1'
const neutral = 'Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно.'
const watchlist = 'Вы уже добавили эту игру в желаемое — следим за датой выхода.'

function groundedRecommendation(
  name: string,
  source: Recommendation['explanationSource'],
  explanation: string,
  evidence: Recommendation['evidence'],
  wishlist = false,
): Recommendation {
  return {
    ...recommendation(name, wishlist),
    name,
    explanationSource: source,
    generatedExplanation: source === 'ai',
    explanation,
    evidence,
    wishlisted: wishlist,
  }
}

function groundedOverview(items: Recommendation[]): RecommendationOverview {
  const value = overviewFixture(0, 0)
  const discoveries = items.filter((item) => !item.wishlisted)
  const wishlist = items.filter((item) => item.wishlisted)
  value.discoveries = { featured: discoveries[0] ?? null, items: discoveries.slice(1), total: discoveries.length }
  value.wishlist = { ...value.wishlist, items: wishlist, total: wishlist.length }
  value.calendar.tba = items
  return value
}

async function openGroundedFixture(page: Page, overview: RecommendationOverview, jobs: unknown[] = []) {
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  await page.route('**/api/profile', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFixture()) }))
  await page.route('**/api/recommendations/overview', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(overview) }))
  await page.route('**/api/jobs', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(jobs) }))
  await page.route('**/api/preferences/adjustments?includeInactive=true', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }))
  await page.goto('/')
}

async function setExplanationMode(request: APIRequestContext, mode: string) {
  const response = await request.post(`${fake}/__test/openai-explanation-mode?mode=${mode}`)
  expect(response.status()).toBe(204)
}

async function fakeExplanation(request: APIRequestContext) {
  return request.post(`${fake}/v1/responses`, {
    data: {
      model: 'fixture-model',
      input: JSON.stringify({
        game_name: 'Fixture Grounded Game',
        evidence: [{ key: 'feature-overlap-1', value: 'Совпадают темы и механики: horror, exploration.' }],
      }),
      text: { format: { name: 'recommendation-explanation-v2' } },
    },
  })
}

test.beforeEach(async ({ request }) => {
  expect((await request.post(`${fake}/__test/reset`)).status()).toBe(204)
})

test.afterEach(async ({ request }) => {
  expect((await request.post(`${fake}/__test/reset`)).status()).toBe(204)
})

test('renders distinct deterministic, wishlist-only, and neutral explanations once', async ({ page }) => {
  const horror = 'Совпадают темы и механики: horror, exploration.'
  const tactics = 'Игры с похожим профилем: Into the Breach.'
  const overview = groundedOverview([
    groundedRecommendation('Grounded Horror', 'deterministic', horror, [{ id: 'feature-overlap-1', kind: 'feature_overlap', text: horror }]),
    groundedRecommendation('Grounded Tactics', 'deterministic', tactics, [{ id: 'similar-game-1', kind: 'similar_game', text: tactics }]),
    groundedRecommendation('Wishlist Watch', 'deterministic', watchlist, [{ id: 'wishlist-context-1', kind: 'wishlist_context', text: watchlist }], true),
    groundedRecommendation('Neutral Candidate', 'none', neutral, []),
  ])

  await openGroundedFixture(page, overview)

  const horrorCard = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: 'Grounded Horror' }) }).first()
  const tacticsCard = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: 'Grounded Tactics' }) }).first()
  const wishlistCard = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: 'Wishlist Watch' }) }).first()
  const neutralCard = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: 'Neutral Candidate' }) }).first()

  await expect(horrorCard.getByText(horror, { exact: true })).toHaveCount(1)
  await expect(tacticsCard.getByText(tactics, { exact: true })).toHaveCount(1)
  await expect(horrorCard.locator('.reason')).toHaveCount(0)
  await expect(tacticsCard.locator('.reason')).toHaveCount(0)
  await expect(wishlistCard.getByText(watchlist, { exact: true })).toHaveCount(1)
  await expect(wishlistCard.getByRole('list', { name: 'Почему вам может понравиться' })).toHaveCount(0)
  await expect(neutralCard.getByText(neutral, { exact: true })).toHaveCount(1)
  await expect(page.getByText(/Добавляет новое|новый опыт|расширяет ваши интересы/i)).toHaveCount(0)
})

test('renders AI prose with traceable evidence and safe aggregate diagnostics', async ({ page, request }) => {
  await setExplanationMode(request, 'success')
  const response = await fakeExplanation(request)
  expect(response.ok()).toBeTruthy()
  const envelope = await response.json() as { output: Array<{ content: Array<{ text: string }> }> }
  const output = JSON.parse(envelope.output[0].content[0].text) as { evidence_keys: string[] }
  expect(output.evidence_keys).toEqual(['feature-overlap-1'])

  const reason = 'Игра продолжает подтверждённый интерес к исследованию и психологическому хоррору.'
  const evidence = 'Совпадают темы и механики: psychological horror, exploration.'
  const overview = groundedOverview([
    groundedRecommendation('AI Grounded Game', 'ai', reason, [{ id: 'feature-overlap-1', kind: 'feature_overlap', text: evidence }]),
  ])
  const jobs = [{
    id: '00000000-0000-4000-8000-000000000001', kind: 'recommendations.rebuild', stage: '', status: 'succeeded', attempts: 1,
    createdAt: '2026-10-18T10:00:00Z', updatedAt: '2026-10-18T10:00:01Z',
    explanations: {
      aiGenerated: 1, deterministic: 2, none: 1,
      failureReasons: { rate_limited: 1, invalid_citation: 1, unknown_provider_body: 99 },
      gameName: 'Private Fixture Name', prompt: 'private prompt', providerBody: 'private response', sourceGameId: '00000000-0000-4000-8000-000000000099',
    },
  }]

  await openGroundedFixture(page, overview, jobs)
  const card = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: 'AI Grounded Game' }) }).first()
  await expect(card.getByText(reason, { exact: true })).toHaveCount(1)
  await expect(card.getByText('AI-сводка по вашим игровым данным', { exact: true })).toBeVisible()
  await card.getByText('На чём основано', { exact: true }).click()
  await expect(card.getByText(evidence, { exact: true })).toBeVisible()

  const diagnostics = page.locator('.explanation-diagnostics')
  await expect(diagnostics).toContainText('AI-сводки1')
  await expect(diagnostics).toContainText('По правилам2')
  await expect(diagnostics).toContainText('Нейтральные1')
  await expect(diagnostics).toContainText('Лимит запросов1')
  await expect(diagnostics).toContainText('Некорректные ссылки на основания1')
  await expect(diagnostics).not.toContainText(/Private Fixture Name|00000000-0000-4000-8000-000000000099|private prompt|private response|unknown_provider_body/i)
})

for (const fixture of [
  ['rate_limited', 429, 'rate_limited'],
  ['invalid_citation', 200, 'invalid_citation'],
  ['unsafe_text', 200, 'unsafe_text'],
  ['provider_unavailable', 503, 'provider_unavailable'],
] as const) {
  test(`keeps a specific deterministic fallback for ${fixture[0]}`, async ({ page, request }) => {
    const [mode, status, reason] = fixture
    await setExplanationMode(request, mode)
    expect((await fakeExplanation(request)).status()).toBe(status)

    const fallback = 'Совпадают темы и механики: survival horror, exploration.'
    const overview = groundedOverview([
      groundedRecommendation(`Fallback ${mode}`, 'deterministic', fallback, [{ id: 'feature-overlap-1', kind: 'feature_overlap', text: fallback }]),
    ])
    const failureReasons: Partial<Record<ExplanationFailureReason, number>> = { [reason]: 1 }
    const jobs = [{
      id: `00000000-0000-4000-8000-00000000000${fixture[1] === 200 ? 2 : 3}`, kind: 'recommendations.rebuild', stage: '', status: 'succeeded', attempts: 1,
      createdAt: '2026-10-18T10:00:00Z', updatedAt: '2026-10-18T10:00:01Z',
      explanations: { aiGenerated: 0, deterministic: 1, none: 0, failureReasons },
    }]
    await openGroundedFixture(page, overview, jobs)

    const card = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: `Fallback ${mode}` }) }).first()
    await expect(card.getByText(fallback, { exact: true })).toHaveCount(1)
    await expect(card.getByText('По вашим игровым данным', { exact: true })).toBeVisible()
    await expect(card.getByText(/Добавляет новое|новый опыт|расширяет ваши интересы/i)).toHaveCount(0)
  })
}

test('persists real backend AI and classified fallback outcomes', async ({ page, request }) => {
  test.skip(openAIDisabled, 'covered by the dedicated no-key startup scenario')
  test.setTimeout(240_000)
  expect((await request.post(`${fake}/__test/steam-privacy?mode=public`)).status()).toBe(204)
  expect((await request.post(`${fake}/__test/wishlist-mode?mode=public`)).status()).toBe(204)
  expect((await request.post(`${fake}/__test/catalog-mode?mode=healthy`)).status()).toBe(204)
  await setExplanationMode(request, 'success')

  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const currentUser = await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
  expect((await page.request.post(`${api}/api/sync/preflight`, { headers: { 'X-CSRF-Token': currentUser.csrfToken } })).ok()).toBeTruthy()
  const syncResponse = await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': currentUser.csrfToken } })
  expect(syncResponse.status()).toBe(202)
  const syncID = (await syncResponse.json() as { jobId: string }).jobId
  await expect.poll(async () => {
    const response = await page.request.get(`${api}/api/sync/${syncID}`)
    return (await response.json() as { status: string }).status
  }, { timeout: 120_000 }).toBe('succeeded')

  const loadOverview = async () => (await (await page.request.get(`${api}/api/recommendations/overview`)).json()) as RecommendationOverview
  const recommendations = (overview: RecommendationOverview) => [overview.discoveries.featured, ...overview.discoveries.items, ...overview.wishlist.items].filter((item): item is Recommendation => item !== null)
  let overview = await loadOverview()
  const aiItems = recommendations(overview).filter((item) => item.explanationSource === 'ai')
  expect(aiItems.length).toBeGreaterThan(0)
  expect(aiItems.every((item) => item.generatedExplanation && item.evidence.length > 0)).toBeTruthy()
  expect(JSON.stringify(aiItems)).not.toMatch(/Добавляет новое|новый опыт|расширяет ваши интересы/i)

  async function rebuildWith(mode: 'rate_limited' | 'invalid_citation' | 'unsafe_text') {
    await setExplanationMode(request, mode)
    const target = recommendations(overview).find((item) => item.explanationSource !== 'none')
    if (!target) throw new Error(`fixture needs an explainable recommendation before ${mode}`)
    const response = await page.request.post(`${api}/api/recommendations/${target.id}/feedback`, {
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': currentUser.csrfToken },
      data: { action: 'like', reason: 'setting', idempotencyKey: `explanation-${mode}` },
    })
    expect(response.status()).toBe(202)
    const accepted = await response.json() as { rebuildJobId: string; previousRankingVersion: number }
    await expect.poll(async () => {
      const status = await page.request.get(`${api}/api/rankings/${accepted.previousRankingVersion}/diff?jobId=${accepted.rebuildJobId}`)
      return (await status.json() as { rebuildStatus: string }).rebuildStatus
    }, { timeout: 120_000 }).toBe('succeeded')
    overview = await loadOverview()
    const items = recommendations(overview)
    expect(items.some((item) => item.explanationSource === 'deterministic' && item.evidence.length > 0)).toBeTruthy()
    const jobs = await (await page.request.get(`${api}/api/jobs`)).json() as Array<{ kind: string; explanations?: { failureReasons: Record<string, number> } }>
    expect(jobs.some((job) => job.kind === 'recommendations.rebuild' && (job.explanations?.failureReasons[mode] ?? 0) > 0)).toBeTruthy()
  }

  await rebuildWith('rate_limited')
  await rebuildWith('invalid_citation')
  await rebuildWith('unsafe_text')

  await page.goto('/')
  const fallback = recommendations(overview).find((item) => item.explanationSource === 'deterministic' && item.evidence.length > 0)
  if (!fallback) throw new Error('fixture needs a persisted deterministic fallback')
  const card = page.locator('article.recommendation-card').filter({ has: page.getByRole('heading', { name: fallback.name }) }).first()
  await expect(card.getByText('По вашим игровым данным', { exact: true })).toBeVisible()
  await expect(card).not.toContainText(/Добавляет новое|новый опыт|расширяет ваши интересы/i)
})

test('publishes deterministic evidence with disabled diagnostics on no-key startup', async ({ page }) => {
  test.skip(!openAIDisabled, 'requires the explicit no-key server startup')
  test.setTimeout(180_000)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const currentUser = await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
  const before = await (await page.request.get(`${api}/api/recommendations/overview`)).json() as RecommendationOverview
  const previousItems = [before.discoveries.featured, ...before.discoveries.items, ...before.wishlist.items].filter((item): item is Recommendation => item !== null)
  const target = previousItems.find((item) => item.evidence.some((entry) => entry.kind !== 'wishlist_context'))
  if (!target) throw new Error('no-key scenario requires the preceding enabled run with stored evidence')
  const feedback = await page.request.post(`${api}/api/recommendations/${target.id}/feedback`, {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': currentUser.csrfToken },
    data: { action: 'like', reason: 'genre', idempotencyKey: 'explanation-no-key-rebuild' },
  })
  expect(feedback.status()).toBe(202)
  const accepted = await feedback.json() as { rebuildJobId: string; previousRankingVersion: number }
  await expect.poll(async () => {
    const response = await page.request.get(`${api}/api/rankings/${accepted.previousRankingVersion}/diff?jobId=${accepted.rebuildJobId}`)
    return (await response.json() as { rebuildStatus: string }).rebuildStatus
  }, { timeout: 120_000 }).toBe('succeeded')

  const overview = await (await page.request.get(`${api}/api/recommendations/overview`)).json() as RecommendationOverview
  const items = [overview.discoveries.featured, ...overview.discoveries.items, ...overview.wishlist.items].filter((item): item is Recommendation => item !== null)
  expect(items.some((item) => item.explanationSource === 'ai')).toBeFalsy()
  expect(items.some((item) => item.explanationSource === 'deterministic' && item.evidence.length > 0)).toBeTruthy()
  const jobs = await (await page.request.get(`${api}/api/jobs`)).json() as Array<{ kind: string; explanations?: { failureReasons: Record<string, number> } }>
  expect(jobs.some((job) => job.kind === 'recommendations.rebuild' && (job.explanations?.failureReasons.disabled ?? 0) > 0)).toBeTruthy()
})
