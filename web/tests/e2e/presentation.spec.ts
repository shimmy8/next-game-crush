import { expect, test, type Page } from '@playwright/test'
import type { PreferenceProfileData, RecommendationOverview } from '../../src/api/types'
import { overviewFixture, profileFixture } from '../../src/test/overviewFixture'

const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'

function presentationOverview(kind: 'mixed' | 'discovery' | 'wishlist' | 'empty' | 'stale'): RecommendationOverview {
  const value = overviewFixture(kind === 'wishlist' || kind === 'empty' ? 0 : 8, kind === 'discovery' || kind === 'empty' ? 0 : 8)
  value.catalogStatus = {
    status: kind === 'stale' ? 'unavailable' : 'ready', activeDiscoveredGames: kind === 'wishlist' || kind === 'empty' ? 0 : 8,
    lastSuccessfulScanAt: '2026-10-18T10:00:00Z', backfillCompletedAt: kind === 'stale' ? null : '2026-10-18T10:00:00Z',
  }
  if (kind === 'stale') {
    value.wishlist.status = 'stale'
    value.wishlist.availability = 'request_failed'
    value.wishlist.degradedReason = 'request_failed'
  }
  const discoveryExact = value.discoveries.featured
  const wishlistExact = value.wishlist.items[0]
  if (discoveryExact) {
    discoveryExact.release = { precision: 'day', exactDate: '2026-10-21' }
    discoveryExact.score = 85
  }
  if (wishlistExact) {
    wishlistExact.release = { precision: 'day', exactDate: '2026-10-22' }
    wishlistExact.score = 70
  }
  const all = [value.discoveries.featured, ...value.discoveries.items, ...value.wishlist.items].filter((item) => item !== null)
  value.calendar = { exactDates: all.filter((item) => item.release.precision === 'day'), month: [], quarter: [], year: [], comingSoon: [], tba: all.filter((item) => item.release.precision !== 'day') }
  return value
}

async function openFixture(page: Page, overview: RecommendationOverview, profile: PreferenceProfileData = profileFixture()) {
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  await page.route('**/api/profile', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profile) }))
  await page.route('**/api/recommendations/overview', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(overview) }))
  await page.goto('/')
}

for (const scenario of ['mixed', 'discovery', 'wishlist', 'empty', 'stale'] as const) {
  test(`renders ${scenario} presentation state at desktop and 375px`, async ({ page }) => {
    const overview = presentationOverview(scenario)
    await openFixture(page, overview)

    await expect(page.getByRole('heading', { name: 'Новая находка для вас' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Ещё новые игры' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Уже в вашем желаемом' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Охват каталога' })).toBeVisible()
    await expect(page.getByText(`Всего новых игр: ${overview.discoveries.total}`)).toBeVisible()
    await expect(page.getByText(`Всего в желаемом: ${overview.wishlist.total}`)).toBeVisible()

    if (scenario === 'wishlist' || scenario === 'empty') {
      await expect(page.getByText('Пока не нашли новых игр вне вашего списка желаемого')).toBeVisible()
    } else {
      await expect(page.getByText('Discovery 1', { exact: true }).first()).toBeVisible()
      expect(await page.locator('.recommendations-section:not(.wishlist-section) .recommendation-card').count()).toBe(6)
    }
    if (scenario === 'discovery' || scenario === 'empty') {
      await expect(page.getByText('В желаемом нет подходящих будущих релизов.')).toBeVisible()
    } else {
      await expect(page.getByText('Wishlist 1', { exact: true }).first()).toBeVisible()
      expect(await page.locator('.wishlist-section .recommendation-card').count()).toBe(6)
    }
    if (scenario === 'stale') {
      await expect(page.getByText('Показываем сохранённый список желаемого. Обновить его пока не удалось.')).toBeVisible()
      await expect(page.getByText('Обновление каталога недоступно')).toBeVisible()
    }

    if (scenario === 'mixed') {
      const calendar = page.locator('.calendar-section')
      await calendar.getByLabel('Источник игр').selectOption('wishlist')
      await calendar.getByLabel('Точность даты').selectOption('day')
      await expect(calendar.getByRole('link', { name: /Wishlist 1 — открыть в Steam/ })).toBeVisible()
      await expect(calendar.getByRole('link', { name: /Discovery 1 — открыть в Steam/ })).toHaveCount(0)
      await calendar.getByLabel('Источник игр').selectOption('discoveries')
      await calendar.getByLabel('Минимальное совпадение').fill('90')
      await expect(calendar.getByRole('link', { name: /Discovery 1 — открыть в Steam/ })).toHaveCount(0)
    }

    await page.setViewportSize({ width: 375, height: 812 })
    await expect(page.getByRole('heading', { name: 'Уже в вашем желаемом' })).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy()
  })
}

test('retains saved collections when an overview refresh fails', async ({ page }) => {
  const overview = presentationOverview('mixed')
  const profile = profileFixture()
  let failOverview = false
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  await page.route('**/api/profile', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profile) }))
  await page.route('**/api/recommendations/overview', (route) => failOverview
    ? route.fulfill({ status: 503, body: 'fixture unavailable' })
    : route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(overview) }))
  await page.route('**/api/recommendations/*/feedback', (route) => route.fulfill({ status: 202, contentType: 'application/json', body: JSON.stringify({ eventId: 'event', rebuildJobId: 'job', previousRankingVersion: 1 }) }))
  await page.route('**/api/rankings/1/diff?jobId=job', (route) => {
    failOverview = true
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ rebuildStatus: 'succeeded', previousVersion: 1, currentVersion: 2, added: [], removed: [], reordered: [] }) })
  })
  await page.goto('/')
  await page.getByRole('button', { name: 'Не моё' }).first().click()
  await page.getByRole('button', { name: 'Сохранить отзыв' }).first().click()
  await expect(page.getByText('Показываем сохранённые рекомендации.')).toBeVisible()
  await expect(page.getByText('Discovery 2', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('Wishlist 1', { exact: true }).first()).toBeVisible()
})
