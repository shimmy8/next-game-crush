import { expect, test } from '@playwright/test'

test('complete fixture-authenticated product loop', async ({ page, request }) => {
  const api = 'http://127.0.0.1:18080'
  const items = (calendar: Record<string, Array<{ id: string; gameId: string }> | null>) =>
    ['exactDates', 'month', 'quarter', 'year', 'comingSoon', 'tba'].flatMap((key) => calendar[key] ?? [])
  await request.delete('http://127.0.0.1:8025/api/v1/messages')
  const login = await page.request.post(`${api}/auth/fixture`)
  expect(login.ok()).toBeTruthy()
  await page.goto('/')
  await expect(page.getByText('Во что влюбиться дальше')).toBeVisible()

  const me = await page.request.get(`${api}/api/me`)
  const user = await me.json() as { csrfToken: string }
  expect((await request.post('http://127.0.0.1:18090/__test/steam-privacy?mode=private')).status()).toBe(204)
  const syncRequests: string[] = []
  page.on('request', (value) => {
    if (value.url().includes('/api/sync')) syncRequests.push(new URL(value.url()).pathname)
  })
  await page.getByRole('button', { name: 'Синхронизировать' }).click()
  await expect(page.getByText('Профиль Steam закрыт')).toBeVisible()
  const blockedJobs = await (await page.request.get(`${api}/api/jobs`)).json() as Array<{ kind: string }>
  expect(blockedJobs.filter((job) => job.kind === 'steam.sync')).toHaveLength(0)

  expect((await request.post('http://127.0.0.1:18090/__test/steam-privacy?mode=public')).status()).toBe(204)
  await page.getByRole('button', { name: 'Проверить снова' }).click()
  await expect(page.getByText('Fixture Player')).toBeVisible()
  await expect(page.getByText('Вы любите исследование и понятные системы.').first()).toBeVisible({ timeout: 90_000 })
  const successfulPreflight = syncRequests.lastIndexOf('/api/sync/preflight')
  const browserStart = syncRequests.lastIndexOf('/api/sync')
  expect(successfulPreflight).toBeGreaterThanOrEqual(0)
  expect(browserStart).toBeGreaterThan(successfulPreflight)

  const jobs = await (await page.request.get(`${api}/api/jobs`)).json() as Array<{ id: string; kind: string }>
  const first = jobs.find((job) => job.kind === 'steam.sync')
  expect(first).toBeTruthy()
  await expect.poll(async () => {
    const status = await page.request.get(`${api}/api/sync/${first!.id}`)
    return (await status.json() as { status: string }).status
  }, { timeout: 90_000 }).toBe('succeeded')
  const firstStatus = await (await page.request.get(`${api}/api/sync/${first!.id}`)).json() as { stages: Array<{ kind: string; stage: string; status: string }> }
  expect(firstStatus.stages.some((stage) => /achievement/i.test(`${stage.kind} ${stage.stage}`) && stage.status === 'succeeded')).toBeTruthy()
  const firstCalendar = await (await page.request.get(`${api}/api/calendar`)).json() as Record<string, Array<{ id: string; gameId: string }> | null>

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

  const calendar = await page.request.get(`${api}/api/calendar`)
  const buckets = await calendar.json() as Record<string, Array<{ id: string; gameId: string }> | null>
  expect(items(buckets).length).toBeGreaterThan(0)
  expect(buckets.exactDates?.length).toBeGreaterThan(0)
  expect(buckets.month?.length).toBeGreaterThan(0)
  const latestGames = items(buckets).map((item) => item.gameId)
  expect(new Set(latestGames).size).toBe(latestGames.length)
  expect(latestGames.length).toBe(items(firstCalendar).length)
  expect([...latestGames].sort()).toEqual(items(firstCalendar).map((item) => item.gameId).sort())

  await page.getByRole('button', { name: 'Нравится' }).first().click()
  await page.getByLabel('Причина').first().selectOption('genre')
  await page.getByLabel('Комментарий').first().fill('Больше исследований')
  await page.getByRole('button', { name: 'Сохранить отзыв' }).first().click()
  await expect(page.getByText(/Рекомендации обновлены · версия/)).toBeVisible({ timeout: 90_000 })

  const digest = await page.request.post(`${api}/api/digests/test`, { headers: { 'X-CSRF-Token': user.csrfToken } })
  expect(digest.status()).toBe(202)
  const digestID = (await digest.json() as { deliveryId: string }).deliveryId
  const repeatedDigest = await page.request.post(`${api}/api/digests/test`, { headers: { 'X-CSRF-Token': user.csrfToken } })
  expect((await repeatedDigest.json() as { deliveryId: string }).deliveryId).toBe(digestID)
  let messageID = ''
  await expect.poll(async () => {
    const response = await request.get('http://127.0.0.1:8025/api/v1/messages')
    const body = await response.json() as { messages?: Array<{ ID: string }> }
    messageID = body.messages?.[0]?.ID ?? ''
    return messageID
  }, { timeout: 30_000 }).not.toBe('')
  const mailbox = await (await request.get('http://127.0.0.1:8025/api/v1/messages')).json() as { messages?: Array<{ ID: string }> }
  expect(mailbox.messages).toHaveLength(1)
  const message = await request.get(`http://127.0.0.1:8025/api/v1/message/${messageID}`)
  const source = JSON.stringify(await message.json())
  const token = source.match(/actions\/feedback\/([A-Za-z0-9._-]+)/)?.[1]
  expect(token).toBeTruthy()
  const action = await request.get(`${api}/actions/feedback/${token}`)
  expect(action.status()).toBe(200)
  const replay = await request.get(`${api}/actions/feedback/${token}`)
  expect(await replay.text()).toContain('already saved')

  const remove = await page.request.delete(`${api}/api/data`, { headers: { 'X-CSRF-Token': user.csrfToken } })
  expect(remove.status()).toBe(204)
  expect((await page.request.get(`${api}/api/me`)).status()).toBe(401)
})
