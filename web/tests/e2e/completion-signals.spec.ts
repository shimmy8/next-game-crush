import { execFileSync } from 'node:child_process'
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import type { PreferenceProfileData, Recommendation, RecommendationOverview } from '../../src/api/types'

test.describe.configure({ timeout: 300_000 })

const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
const fake = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'
const database = process.env.E2E_DATABASE_URL ?? 'postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_e2e?sslmode=disable'
const openAIDisabled = process.env.E2E_OPENAI_DISABLED === '1'

function sql(query: string) {
  return execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-Atc', query], { encoding: 'utf8' }).trim()
}

function recommendations(overview: RecommendationOverview) {
  return [overview.discoveries.featured, ...overview.discoveries.items, ...overview.wishlist.items]
    .filter((item): item is Recommendation => item !== null)
}

async function resetAndSync(page: Page, request: APIRequestContext, expectedProjections = 1, resetFake = true) {
  if (resetFake) expect((await request.post(`${fake}/__test/reset`)).status()).toBe(204)
  const existing = await page.request.post(`${api}/auth/fixture`)
  expect(existing.ok()).toBeTruthy()
  const current = await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
  const deletion = await page.request.delete(`${api}/api/data`, { headers: { 'X-CSRF-Token': current.csrfToken } })
  expect([204, 404]).toContain(deletion.status())
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const owner = await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
  const started = await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': owner.csrfToken } })
  expect(started.status()).toBe(202)
  const { jobId } = await started.json() as { jobId: string }
  await expect.poll(async () => {
    const response = await page.request.get(`${api}/api/sync/${jobId}`)
    return (await response.json() as { status: string }).status
  }, { timeout: 120_000 }).toBe('succeeded')
  await expect.poll(() => sql(`SELECT count(*) FROM user_game_completion_projections projection JOIN games ON games.id=projection.game_id WHERE games.steam_app_id IN (10,11)`), { timeout: 90_000 }).toBe(String(expectedProjections))
  await page.goto('/')
}

test('completed short game is a persistent historical signal beside high-hour recent play', async ({ page, request }) => {
  test.skip(openAIDisabled, 'enabled-provider scenario')
  await resetAndSync(page, request)

  const profile = await (await page.request.get(`${api}/api/profile`)).json() as PreferenceProfileData
  expect(profile.summaryStatus).toBe('ready')
  expect(profile.summary).toContain('Fixture Short Story — сильный исторический сигнал')
  expect(profile.summary).toContain('недавней активности нет')
  expect(profile.summary).toContain('Fixture Endless Realm')
  await expect(page.getByText(/Fixture Short Story — сильный исторический сигнал/)).toBeVisible()

  const overview = await (await page.request.get(`${api}/api/recommendations/overview`)).json() as RecommendationOverview
  const completionEvidence = recommendations(overview).flatMap((item) => item.evidence).filter((item) => item.kind === 'completion_anchor')
  expect(completionEvidence.some((item) => item.text.includes('Fixture Short Story'))).toBeTruthy()
  expect(sql(`SELECT weight::text||'|'||basis||'|'||ending_count::text FROM user_game_completion_projections projection JOIN games ON games.id=projection.game_id WHERE games.steam_app_id=10`)).toBe('1.5|ending|1')

  const visible = await page.locator('body').innerText()
  expect(visible).not.toMatch(/ручн(?:ая|ую) отметк|пометить.*пройден/i)
  expect(visible).not.toMatch(/completionAffinity|base_game_ending|achievement_semantics|\bENDING\b/)
})

test('lexical ending and historical hundred percent work with OpenAI disabled', async ({ page, request }) => {
  test.skip(!openAIDisabled, 'disabled-provider scenario')
  expect((await request.post(`${fake}/__test/reset`)).status()).toBe(204)
  expect((await request.post(`${fake}/__test/openai-achievement-mode?mode=hundred_percent`)).status()).toBe(204)
  await resetAndSync(page, request, 2, false)

  expect(sql(`SELECT string_agg(classification_source||':'||class,',' ORDER BY classification_source,class) FROM achievement_semantic_classifications classification JOIN games ON games.id=classification.game_id WHERE games.steam_app_id=10`)).toContain('lexical:base_game_ending')
  expect(sql(`SELECT count(*) FROM achievement_semantic_classifications classification JOIN games ON games.id=classification.game_id WHERE games.steam_app_id=10 AND classification_source='unknown' AND class='unknown'`)).not.toBe('0')
  expect(sql(`SELECT weight::text||'|'||basis FROM user_game_completion_projections projection JOIN games ON games.id=projection.game_id WHERE games.steam_app_id=10`)).toBe('1.5|ending')
  expect(sql(`SELECT weight::text||'|'||basis FROM user_game_completion_projections projection JOIN games ON games.id=projection.game_id WHERE games.steam_app_id=11`)).toBe('1.5|hundred_percent')

  const overview = await (await page.request.get(`${api}/api/recommendations/overview`)).json() as RecommendationOverview
  const completionEvidence = recommendations(overview).flatMap((item) => item.evidence).filter((item) => item.kind === 'completion_anchor')
  expect(completionEvidence.some((item) => item.text.includes('Есть сходство с пройденной вами игрой Fixture'))).toBeTruthy()
  await expect(page.getByText(/Есть сходство с пройденной вами игрой Fixture/).first()).toBeVisible()
  await expect(page.getByRole('button', { name: 'Синхронизировать' })).toBeVisible()

  const visible = await page.locator('body').innerText()
  expect(visible).not.toMatch(/ручн(?:ая|ую) отметк|пометить.*пройден/i)
  expect(visible).not.toMatch(/classification_source|base_game_ending|achievement_semantics|\bENDING\b/)
})
