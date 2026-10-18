import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import type { Recommendation } from '../../src/api/types'

const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
const fake = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'

type HealthFixture = { catalogDiscovery: { activeDiscoveredGames: number; status: string; reason?: string } }

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
  for (const path of ['/__test/steam-privacy?mode=public', '/__test/wishlist-mode?mode=public', '/__test/catalog-mode?mode=healthy']) {
    expect((await request.post(`${fake}${path}`)).status()).toBe(204)
  }
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
  expect(calendar.exactDates.some((item) => item.steamAppId === 424242 && item.release.exactDate === '2026-11-15')).toBeTruthy()

  await expect.poll(async () => (await loadHealth(page)).catalogDiscovery.status, { timeout: 90_000 }).toBe('degraded')
  const afterFailure = await loadRecommendations(page)
  expect(afterFailure.map((item) => item.id)).toEqual(beforeFailure.map((item) => item.id))

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Fixture Discovered Co-op' }).first()).toBeVisible()
  await expect(page.getByText('Обновление каталога недоступно')).toBeVisible()
  await expect(page.getByText('Последние сохранённые рекомендации остаются видны.')).toBeVisible()
})
