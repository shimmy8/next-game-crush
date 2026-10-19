import { expect, test } from '@playwright/test'

test.describe.configure({ timeout: 300_000 })

test('complete fixture-authenticated product loop', async ({ page, request }) => {
  const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
  const fake = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'
  const web = process.env.E2E_WEB_URL ?? 'http://127.0.0.1:4173'
  const mailpit = process.env.E2E_MAILPIT_URL ?? 'http://127.0.0.1:8025'
  type CalendarItem = {
    id: string
    gameId: string
    steamAppId: number
    name: string
    platforms?: { windows: boolean; mac: boolean; linux: boolean }
    deckCompatibility: 'verified' | 'playable' | 'unsupported' | 'unknown'
  }
  type Overview = {
    discoveries: { featured: CalendarItem | null; items: CalendarItem[]; total: number }
    wishlist: { items: CalendarItem[]; total: number; status: string; availability: string; lastSuccessfulFetchAt: string | null; degradedReason: string | null }
  }
  const items = (calendar: Record<string, CalendarItem[] | null>) =>
    ['exactDates', 'month', 'quarter', 'year', 'comingSoon', 'tba'].flatMap((key) => calendar[key] ?? [])
  await request.delete(`${mailpit}/api/v1/messages`)
  expect((await request.post(`${fake}/__test/wishlist-mode?mode=public`)).status()).toBe(204)
  expect((await request.post(`${fake}/__test/catalog-mode?mode=healthy`)).status()).toBe(204)
  const existingLogin = await page.request.post(`${api}/auth/fixture`)
  expect(existingLogin.ok()).toBeTruthy()
  const existingUser = await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
  expect((await page.request.delete(`${api}/api/data`, { headers: { 'X-CSRF-Token': existingUser.csrfToken } })).status()).toBe(204)
  const login = await page.request.post(`${api}/auth/fixture`)
  expect(login.ok()).toBeTruthy()
  await page.goto('/')
  await expect(page.getByText('Во что влюбиться дальше')).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'Новая находка для вас' })).toBeVisible()

  const me = await page.request.get(`${api}/api/me`)
  const user = await me.json() as { csrfToken: string }
  const loadOverview = async () => (await (await page.request.get(`${api}/api/recommendations/overview`)).json()) as Overview
  const runAPISync = async () => {
    const response = await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': user.csrfToken } })
    expect(response.status()).toBe(202)
    const { jobId } = await response.json() as { jobId: string }
    await expect.poll(async () => (await (await page.request.get(`${api}/api/sync/${jobId}`)).json() as { status: string }).status, { timeout: 90_000 }).toBe('succeeded')
  }
  expect((await request.post(`${fake}/__test/steam-privacy?mode=private`)).status()).toBe(204)
  const syncRequests: string[] = []
  page.on('request', (value) => {
    if (value.url().includes('/api/sync')) syncRequests.push(new URL(value.url()).pathname)
  })
  await page.getByRole('button', { name: 'Синхронизировать' }).click()
  await expect(page.getByText('Профиль Steam закрыт')).toBeVisible()
  const blockedJobs = await (await page.request.get(`${api}/api/jobs`)).json() as Array<{ kind: string }>
  expect(blockedJobs.filter((job) => job.kind === 'steam.sync')).toHaveLength(0)

  expect((await request.post(`${fake}/__test/steam-privacy?mode=public`)).status()).toBe(204)
  const syncResponsePromise = page.waitForResponse((response) => response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/sync')
  await page.getByRole('button', { name: 'Проверить снова' }).click()
  const syncResponse = await syncResponsePromise
  expect(syncResponse.status()).toBe(202)
  const { jobId } = await syncResponse.json() as { jobId: string }
  await expect(page.getByText('Fixture Player')).toBeVisible()
  await expect(page.getByText(/Fixture Short Story — сильный исторический сигнал/).first()).toBeVisible({ timeout: 90_000 })
  const successfulPreflight = syncRequests.lastIndexOf('/api/sync/preflight')
  const browserStart = syncRequests.lastIndexOf('/api/sync')
  expect(successfulPreflight).toBeGreaterThanOrEqual(0)
  expect(browserStart).toBeGreaterThan(successfulPreflight)

  await expect.poll(async () => {
    const status = await page.request.get(`${api}/api/sync/${jobId}`)
    return (await status.json() as { status: string }).status
  }, { timeout: 90_000 }).toBe('succeeded')
  const firstStatus = await (await page.request.get(`${api}/api/sync/${jobId}`)).json() as { stages: Array<{ kind: string; stage: string; status: string }> }
  expect(firstStatus.stages.some((stage) => /achievement/i.test(`${stage.kind} ${stage.stage}`) && stage.status === 'succeeded')).toBeTruthy()
  // User sync intentionally does not await optional catalog-lane metadata.
  // Wait for the Search publications, all fixture wishlist releases and the
  // catalog-triggered successor ranking before taking the idempotency snapshot.
  const wishlistReleaseAppIDs = Array.from({ length: 8 }, (_, index) => 300001 + index)
  await expect.poll(async () => {
    const health = await (await page.request.get(`${api}/api/health`)).json() as {
      catalogSearch: {
        status: string
        lastReconciliationAt: string | null
      }
    }
    const jobs = await (await page.request.get(`${api}/api/jobs`)).json() as Array<{ kind: string; status: string }>
    const calendar = await (await page.request.get(`${api}/api/calendar`)).json() as Record<string, CalendarItem[] | null>
    const completedRankings = jobs.filter((entry) => entry.kind === 'recommendations.rebuild' && entry.status === 'succeeded').length
    const searchSettled = health.catalogSearch.status === 'ready' && health.catalogSearch.lastReconciliationAt !== null
    const visibleAppIDs = new Set(items(calendar).map((entry) => entry.steamAppId))
    const metadataSettled = wishlistReleaseAppIDs.every((appID) => visibleAppIDs.has(appID))
    return searchSettled && metadataSettled ? completedRankings : 0
  }, { timeout: 120_000 }).toBeGreaterThanOrEqual(2)
  const firstCalendar = await (await page.request.get(`${api}/api/calendar`)).json() as Record<string, CalendarItem[] | null>

  const repeated = await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': user.csrfToken } })
  const second = await repeated.json() as { jobId: string }
  await expect.poll(async () => {
    const status = await page.request.get(`${api}/api/sync/${second.jobId}`)
    return (await status.json() as { status: string }).status
  }, { timeout: 90_000 }).toBe('succeeded')
  await page.reload()
  await expect(page.getByText('В этом месяце')).toBeVisible()
  await expect(page.getByText('В квартале')).toBeVisible()
  await expect(page.getByText('Скоро').first()).toBeVisible()

  const overview = await loadOverview()
  expect(overview.discoveries.total).toBeGreaterThanOrEqual(8)
  expect(overview.wishlist.total).toBeGreaterThanOrEqual(8)
  expect([overview.discoveries.featured, ...overview.discoveries.items].filter(Boolean)).toHaveLength(7)
  expect(overview.wishlist.items).toHaveLength(6)
  expect(overview.discoveries.items.every((item) => !overview.wishlist.items.some((wishlist) => wishlist.gameId === item.gameId))).toBeTruthy()
  expect(await page.locator('.recommendations-section:not(.wishlist-section) .recommendation-card').count()).toBe(6)
  expect(await page.locator('.wishlist-section .recommendation-card').count()).toBe(6)

  const calendar = await page.request.get(`${api}/api/calendar`)
  const buckets = await calendar.json() as Record<string, CalendarItem[] | null>
  expect(items(buckets).length).toBeGreaterThan(0)
  expect(buckets.exactDates?.length).toBeGreaterThan(0)
  expect(buckets.month?.length).toBeGreaterThan(0)
  const latestGames = items(buckets).map((item) => item.gameId)
  expect(new Set(latestGames).size).toBe(latestGames.length)
  expect(latestGames.length).toBe(items(firstCalendar).length)
  expect([...latestGames].sort()).toEqual(items(firstCalendar).map((item) => item.gameId).sort())

  const target = items(buckets).find((item) => item.steamAppId === 4898190)
  if (!target) throw new Error('fixture calendar must contain AppID 4898190')
  expect(target).toMatchObject({ platforms: { windows: true, mac: true, linux: true }, deckCompatibility: 'verified' })
  const platformNegative = items(buckets).find((item) => item.steamAppId === 5179670)
  if (!platformNegative) throw new Error('fixture calendar must contain AppID 5179670')
  expect(platformNegative).toMatchObject({ platforms: { windows: true, mac: false, linux: true }, deckCompatibility: 'verified' })
  const deckNegative = items(buckets).find((item) => item.steamAppId === 5347650)
  if (!deckNegative) throw new Error('fixture calendar must contain AppID 5347650')
  expect(deckNegative).toMatchObject({ platforms: { windows: true, mac: true, linux: true }, deckCompatibility: 'unsupported' })
  const calendarSection = page.locator('.calendar-section')
  await calendarSection.getByRole('button', { name: 'Следующий месяц' }).click()
  await calendarSection.getByRole('button', { name: 'Следующий месяц' }).click()
  const targetLink = calendarSection.getByRole('link', { name: new RegExp(`^${target.name} — открыть в Steam`) })
  await expect(targetLink).toBeVisible()
  await expect(calendarSection.getByRole('link', { name: new RegExp(`^${platformNegative.name} — открыть в Steam`) })).toBeVisible()
  await expect(calendarSection.getByRole('link', { name: new RegExp(`^${deckNegative.name} — открыть в Steam`) })).toBeVisible()
  const compatibility = calendarSection.getByRole('article').filter({ has: targetLink }).getByRole('group', { name: 'Совместимость игры' })
  await expect(compatibility).toContainText('Windows')
  await expect(compatibility).toContainText(/Steam Deck:/)
  await calendarSection.getByRole('combobox', { name: 'Платформа' }).selectOption('mac')
  await calendarSection.getByRole('combobox', { name: 'Steam Deck' }).selectOption('verified')
  await expect(calendarSection.getByRole('link', { name: new RegExp(`^${target.name} — открыть в Steam`) })).toBeVisible()
  await expect(calendarSection.getByRole('link', { name: new RegExp(`^${platformNegative.name} — открыть в Steam`) })).toHaveCount(0)
  await expect(calendarSection.getByRole('link', { name: new RegExp(`^${deckNegative.name} — открыть в Steam`) })).toHaveCount(0)

  const launcher = calendarSection.getByRole('button', { name: 'Оценить' }).first()
  await page.setViewportSize({ width: 375, height: 812 })
  await launcher.focus()
  await page.keyboard.press('Enter')
  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Закрыть' })).toBeFocused()
  await page.keyboard.press('Shift+Tab')
  expect(await dialog.evaluate((element) => element.contains(document.activeElement))).toBeTruthy()
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(launcher).toBeFocused()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy()
  await page.setViewportSize({ width: 1280, height: 900 })

  const beforeFeedback = await loadOverview()
  await page.getByRole('button', { name: 'Нравится' }).first().click()
  await page.getByLabel('Жанр').first().check()
  await page.getByLabel('Комментарий').first().fill('Больше исследований')
  await page.getByRole('button', { name: 'Сохранить отзыв' }).first().click({ noWaitAfter: true })
  await expect(page.getByText(/Рекомендации обновлены · версия/)).toBeVisible({ timeout: 90_000 })
  const afterDiscoveryFeedback = await loadOverview()
  expect(afterDiscoveryFeedback.discoveries.items.every((item) => !afterDiscoveryFeedback.wishlist.items.some((wishlist) => wishlist.gameId === item.gameId))).toBeTruthy()
  expect(afterDiscoveryFeedback.wishlist.total).toBe(beforeFeedback.wishlist.total)

  const wishlistSection = page.locator('.wishlist-section')
  await wishlistSection.getByRole('button', { name: 'Не моё' }).first().click()
  await wishlistSection.getByLabel('Мультиплеер').first().check()
  await wishlistSection.getByLabel('Комментарий').first().fill('не люблю коопы')
  await wishlistSection.getByRole('button', { name: 'Сохранить отзыв' }).first().click({ noWaitAfter: true })
  await expect(page.getByText('Кооперативные игры', { exact: true }).first()).toBeVisible({ timeout: 90_000 })
  const activeAdjustments = await (await page.request.get(`${api}/api/preference-adjustments?includeInactive=false`)).json() as Array<{ id: string; active: boolean; label: string }>
  expect(activeAdjustments).toHaveLength(1)
  expect(activeAdjustments[0]).toMatchObject({ active: true, label: 'Кооперативные игры' })
  await page.getByRole('button', { name: 'Не учитывать: Кооперативные игры' }).click()
  await expect(page.getByText('Рекомендации обновлены').last()).toBeVisible({ timeout: 90_000 })
  const disableRequests = await Promise.all([
    page.request.delete(`${api}/api/preference-adjustments/${activeAdjustments[0].id}`, { headers: { 'X-CSRF-Token': user.csrfToken } }),
    page.request.delete(`${api}/api/preference-adjustments/${activeAdjustments[0].id}`, { headers: { 'X-CSRF-Token': user.csrfToken } }),
  ])
  expect(disableRequests.map((response) => response.status())).toEqual([202, 202])
  const disabledHistory = await (await page.request.get(`${api}/api/preference-adjustments?includeInactive=true`)).json() as Array<{ id: string; active: boolean }>
  const disabled = disabledHistory.filter((item) => item.id === activeAdjustments[0].id)
  expect(disabled).toHaveLength(1)
  expect(disabled[0]).toMatchObject({ id: activeAdjustments[0].id, active: false })

  const successfulWishlist = await loadOverview()
  const successfulWishlistIDs = successfulWishlist.wishlist.items.map((item) => item.gameId)
  const wishlistSuccessAt = successfulWishlist.wishlist.lastSuccessfulFetchAt
  expect(wishlistSuccessAt).not.toBeNull()
  expect((await request.post(`${fake}/__test/wishlist-mode?mode=unavailable`)).status()).toBe(204)
  await runAPISync()
  let degradedWishlist = await loadOverview()
  expect(degradedWishlist.wishlist).toMatchObject({ status: 'stale', availability: 'unavailable', lastSuccessfulFetchAt: wishlistSuccessAt, degradedReason: 'private_or_unavailable' })
  expect(degradedWishlist.wishlist.items.map((item) => item.gameId).sort()).toEqual([...successfulWishlistIDs].sort())
  expect((await request.post(`${fake}/__test/wishlist-mode?mode=failed`)).status()).toBe(204)
  await runAPISync()
  degradedWishlist = await loadOverview()
  expect(degradedWishlist.wishlist).toMatchObject({ status: 'stale', availability: 'request_failed', lastSuccessfulFetchAt: wishlistSuccessAt, degradedReason: 'request_failed' })
  expect(degradedWishlist.wishlist.items.map((item) => item.gameId).sort()).toEqual([...successfulWishlistIDs].sort())
  await page.reload()
  await expect(page.getByText('Показываем сохранённый список желаемого. Обновить его пока не удалось.')).toBeVisible()
  expect((await request.post(`${fake}/__test/wishlist-mode?mode=empty`)).status()).toBe(204)
  await runAPISync()
  const emptyWishlist = await loadOverview()
  expect(emptyWishlist.wishlist).toMatchObject({ total: 0, status: 'ready', availability: 'available', degradedReason: null })

  const digest = await page.request.post(`${api}/api/digests/test`, { headers: { 'X-CSRF-Token': user.csrfToken } })
  expect(digest.status()).toBe(202)
  const digestID = (await digest.json() as { deliveryId: string }).deliveryId
  const repeatedDigest = await page.request.post(`${api}/api/digests/test`, { headers: { 'X-CSRF-Token': user.csrfToken } })
  expect((await repeatedDigest.json() as { deliveryId: string }).deliveryId).toBe(digestID)
  let messageID = ''
  await expect.poll(async () => {
    const response = await request.get(`${mailpit}/api/v1/messages`)
    const body = await response.json() as { messages?: Array<{ ID: string }> }
    messageID = body.messages?.[0]?.ID ?? ''
    return messageID
  }, { timeout: 30_000 }).not.toBe('')
  const mailbox = await (await request.get(`${mailpit}/api/v1/messages`)).json() as { messages?: Array<{ ID: string }> }
  expect(mailbox.messages).toHaveLength(1)
  const message = await request.get(`${mailpit}/api/v1/message/${messageID}`)
  const source = JSON.stringify(await message.json())
  const token = source.match(/actions\/feedback\/([A-Za-z0-9._-]+)/)?.[1]
  expect(token).toBeTruthy()
  const action = await request.get(`${api}/actions/feedback/${token}`)
  expect(action.status()).toBe(200)
  const replay = await request.get(`${api}/actions/feedback/${token}`)
  expect(await replay.text()).toContain('already saved')

  expect((await page.request.post(`${api}/auth/logout`, { headers: { 'X-CSRF-Token': user.csrfToken } })).status()).toBe(204)
  expect((await page.request.post(`${api}/auth/demo`, { headers: { Origin: web } })).status()).toBe(200)
  const demoMe = await page.request.get(`${api}/api/me`)
  const demoUser = await demoMe.json() as { accessMode: string; csrfToken: string; steamId?: string }
  expect(demoUser).toMatchObject({ accessMode: 'demo' })
  expect(demoUser.steamId).toBeUndefined()
  await page.reload()
  await expect(page.getByRole('status', { name: 'Демо-режим' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Fixture Discovered Co-op' }).first()).toBeVisible()
  await page.getByRole('button', { name: 'Нравится' }).first().click()
  await expect(page.getByText('Хорошая попытка — реакции пока только для владельца.').first()).toBeVisible()
  expect((await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': demoUser.csrfToken } })).status()).toBe(403)

  expect((await page.request.post(`${api}/auth/logout`, { headers: { 'X-CSRF-Token': demoUser.csrfToken } })).status()).toBe(204)
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  const owner = await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
  const remove = await page.request.delete(`${api}/api/data`, { headers: { 'X-CSRF-Token': owner.csrfToken } })
  expect(remove.status()).toBe(204)
  expect((await page.request.get(`${api}/api/me`)).status()).toBe(401)
})
