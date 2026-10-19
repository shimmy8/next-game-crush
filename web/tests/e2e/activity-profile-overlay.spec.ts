import { execFileSync } from 'node:child_process'
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import type { FeedbackAccepted, PreferenceProfileData, Recommendation, RecommendationOverview } from '../../src/api/types'

test.describe.configure({ timeout: 300_000, mode: 'serial' })

const api = process.env.E2E_API_URL ?? 'http://127.0.0.1:18080'
const fake = process.env.E2E_FAKE_URL ?? 'http://127.0.0.1:18090'
const web = process.env.E2E_WEB_URL ?? 'http://127.0.0.1:4173'
const database = process.env.E2E_DATABASE_URL ?? 'postgres://next_game_crush_test:next_game_crush_test@127.0.0.1:55432/next_game_crush_e2e?sslmode=disable'
const openAIDisabled = process.env.E2E_OPENAI_DISABLED === '1'

function sql(query: string) {
  return execFileSync('psql', [database, '-v', 'ON_ERROR_STOP=1', '-Atc', query], { encoding: 'utf8' }).trim()
}

function recommendations(value: RecommendationOverview) {
  return [value.discoveries.featured, ...value.discoveries.items, ...value.wishlist.items]
    .filter((item): item is Recommendation => item !== null)
}

function activityProfileFingerprint() {
  return sql(`SELECT jsonb_build_object(
      'id',p.id,'version',p.version,'summary',p.summary,'summary_source',p.summary_source,
      'metadata',p.algorithm_metadata,'vector',coalesce(pe.embedding::text,''),
      'evidence',coalesce((SELECT jsonb_agg(to_jsonb(e) ORDER BY e.game_id) FROM activity_profile_evidence e WHERE e.profile_id=p.id),'[]'::jsonb)
    )::text
    FROM preference_profiles p LEFT JOIN profile_embeddings pe ON pe.profile_id=p.id AND pe.polarity='positive'
    WHERE p.publication_state='published' ORDER BY p.version DESC LIMIT 1`)
}

async function owner(page: Page) {
  expect((await page.request.post(`${api}/auth/fixture`)).ok()).toBeTruthy()
  return await (await page.request.get(`${api}/api/me`)).json() as { csrfToken: string }
}

async function resetOwner(page: Page, request: APIRequestContext) {
  expect((await request.post(`${fake}/__test/reset`)).status()).toBe(204)
  const existing = await owner(page)
  const deleted = await page.request.delete(`${api}/api/data`, { headers: { 'X-CSRF-Token': existing.csrfToken } })
  expect([204, 404]).toContain(deleted.status())
  return owner(page)
}

async function startSync(page: Page, csrfToken: string) {
  const response = await page.request.post(`${api}/api/sync`, { headers: { 'X-CSRF-Token': csrfToken } })
  expect(response.status()).toBe(202)
  const { jobId } = await response.json() as { jobId: string }
  await expect.poll(async () => (await (await page.request.get(`${api}/api/sync/${jobId}`)).json() as { status: string }).status, { timeout: 120_000 }).toBe('succeeded')
  return jobId
}

async function profile(page: Page) {
  const response = await page.request.get(`${api}/api/profile`)
  expect(response.ok()).toBeTruthy()
  return await response.json() as PreferenceProfileData
}

async function overview(page: Page) {
  return await (await page.request.get(`${api}/api/recommendations/overview`)).json() as RecommendationOverview
}

async function submit(page: Page, csrfToken: string, recommendationId: string, action: 'like' | 'dislike', freeText = '') {
  const response = await page.request.post(`${api}/api/recommendations/${recommendationId}/feedback`, {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken },
    data: { action, reasons: ['multiplayer'], freeText, idempotencyKey: `${action}-${Date.now()}-${Math.random()}` },
  })
  expect(response.status()).toBe(202)
  const accepted = await response.json() as FeedbackAccepted
  await expect.poll(async () => {
    const diff = await page.request.get(`${api}/api/rankings/${accepted.previousRankingVersion}/diff?jobId=${accepted.rebuildJobId}`)
    return diff.ok() ? (await diff.json() as { rebuildStatus: string }).rebuildStatus : 'pending'
  }, { timeout: 120_000 }).toBe('succeeded')
  return accepted
}

test('publishes ranking before a delayed summary and keeps the prior activity profile visible', async ({ page, request }) => {
  test.skip(openAIDisabled, 'enabled-provider SWR scenario')
  const user = await resetOwner(page, request)
  await startSync(page, user.csrfToken)
  const published = await profile(page)
  expect(published.updating).toBe(false)

  const beforeNoise = activityProfileFingerprint()
  sql(`WITH owner AS (SELECT user_id FROM steam_accounts LIMIT 1),
      unplayed AS (INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES(gen_random_uuid(),990001,'Fixture Unplayed Ownership','e2e_fixture',now()) RETURNING id),
      wished AS (INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES(gen_random_uuid(),990002,'Fixture Wishlist Only','e2e_fixture',now()) RETURNING id)
    INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at)
      SELECT owner.user_id,unplayed.id,0,0,'e2e_fixture',now() FROM owner,unplayed;
    INSERT INTO wishlist_entries(user_id,game_id,priority,source,last_successful_fetch_at)
      SELECT owner.user_id,g.id,1,'e2e_fixture',now() FROM owner,games g WHERE g.steam_app_id=990002`)
  expect(sql(`SELECT
    (SELECT count(*) FROM user_games ug JOIN games g ON g.id=ug.game_id WHERE g.steam_app_id=990001 AND ug.playtime_minutes=0 AND ug.recently_played_minutes=0)::text||'|'||
    (SELECT count(*) FROM wishlist_entries w JOIN games g ON g.id=w.game_id WHERE g.steam_app_id=990002)::text`)).toBe('1|1')
  const feedbackOnlyTarget = recommendations(await overview(page))[0]
  expect(feedbackOnlyTarget).toBeDefined()
  await submit(page, user.csrfToken, feedbackOnlyTarget.id, 'like')
  expect(sql(`SELECT count(*) FROM feedback_events WHERE recommendation_id='${feedbackOnlyTarget.id}'`)).toBe('1')
  expect(activityProfileFingerprint()).toBe(beforeNoise)
  expect(await profile(page)).toEqual(published)
  const rankingBeforeDelayedSync = Number(sql(`SELECT max(ranking_version) FROM recommendation_runs WHERE published`))

  expect((await request.post(`${fake}/__test/openai-profile-mode?mode=delayed`)).status()).toBe(204)
  await startSync(page, user.csrfToken)
  await expect.poll(() => sql(`SELECT count(*) FROM preference_profiles WHERE publication_state='building'`)).toBe('1')
  const whileBuilding = await profile(page)
  expect(whileBuilding).toMatchObject({ id: published.id, version: published.version, summary: published.summary, updating: true })
  await page.goto('/')
  await expect(page.getByText('Профиль обновляется')).toBeVisible()
  await expect(page.getByText(published.summary)).toBeVisible()

  const activityBoundary = sql(`SELECT (p.algorithm_metadata->>'evidenceCount')||'|'||count(e.game_id)::text||'|'||
    count(e.game_id) FILTER (WHERE e.playtime_minutes>0 OR e.recently_played_minutes>0 OR e.achieved>0 OR e.completion_weight>0)::text
    FROM preference_profiles p JOIN activity_profile_evidence e ON e.profile_id=p.id
    WHERE p.publication_state='building' GROUP BY p.id,p.algorithm_metadata`)
  expect(activityBoundary).toMatch(/^(\d+)\|\1\|\1$/)
  const rankingDuringSummary = Number(sql(`SELECT max(ranking_version) FROM recommendation_runs WHERE published`))
  expect(rankingDuringSummary).toBeGreaterThan(rankingBeforeDelayedSync)

  expect((await request.post(`${fake}/__test/openai-profile-mode?mode=healthy`)).status()).toBe(204)
  await expect.poll(async () => (await profile(page)).updating, { timeout: 120_000 }).toBe(false)
  const counters = await (await request.get(`${fake}/__test/catalog-counters`)).json() as { profileSummaries: number; profileSummaryMode: string }
  expect(counters.profileSummaries).toBeGreaterThanOrEqual(2)
  expect(counters.profileSummaryMode).toBe('healthy')
})

test('dislike then like advances only overlay and ranking and retires the negative correction', async ({ page, request }) => {
  test.skip(openAIDisabled, 'enabled-provider feedback classification scenario')
  const user = await resetOwner(page, request)
  await startSync(page, user.csrfToken)
  const beforeProfile = await profile(page)
  const before = await overview(page)
  const target = recommendations(before)[0]
  expect(target).toBeDefined()
  sql(`UPDATE game_embeddings SET embedding=(ARRAY[0::real,1::real]||array_fill(0::real,ARRAY[1534]))::vector(1536),
    input_content_hash='e2e-overlay-anchor' WHERE game_id='${target.gameId}'`)
  const overlayNeighborID = sql(`WITH candidate AS (
      INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)
      VALUES(gen_random_uuid(),990003,'AAA Fixture Overlay Neighbor','e2e_fixture',now()) RETURNING id
    ), release AS (
      INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at)
      SELECT gen_random_uuid(),id,'day',current_date+5,(current_date+5)::text,now(),'e2e_fixture',now() FROM candidate
    ), catalog AS (
      INSERT INTO catalog_entries(game_id,origin) SELECT id,'seed' FROM candidate
    ), features AS (
      INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at)
      SELECT candidate.id,source.model,'e2e-overlay-neighbor',source.features,'e2e_fixture',now()
      FROM candidate JOIN game_features source ON source.game_id='${target.gameId}' LIMIT 1
    )
    INSERT INTO game_embeddings(game_id,model,input_content_hash,embedding,source,last_successful_fetch_at)
      SELECT candidate.id,source.model,'e2e-overlay-neighbor',source.embedding,'e2e_fixture',now()
      FROM candidate JOIN game_embeddings source ON source.game_id='${target.gameId}' LIMIT 1
    RETURNING game_id`)
  expect(overlayNeighborID).toMatch(/^[0-9a-f-]{36}$/)
  expect(recommendations(before).some((item) => item.gameId === overlayNeighborID)).toBe(false)
  const beforeOverlay = Number(sql('SELECT max(version) FROM preference_overlays'))
  const beforeRanking = Number(sql('SELECT max(ranking_version) FROM recommendation_runs WHERE published'))

  await submit(page, user.csrfToken, target.id, 'dislike', 'Не люблю коопы')
  const activeAfterDislike = await (await page.request.get(`${api}/api/preference-adjustments?includeInactive=false`)).json() as Array<{ id: string; active: boolean }>
  expect(activeAfterDislike).toHaveLength(1)
  expect(activeAfterDislike[0].active).toBe(true)
  expect(recommendations(await overview(page)).some((item) => item.gameId === overlayNeighborID)).toBe(false)
  const negativeOverlay = Number(sql('SELECT max(version) FROM preference_overlays'))
  const dislikeRanking = Number(sql('SELECT max(ranking_version) FROM recommendation_runs WHERE published'))
  expect(negativeOverlay).toBeGreaterThan(beforeOverlay)
  expect(dislikeRanking).toBeGreaterThan(beforeRanking)

  await submit(page, user.csrfToken, target.id, 'like')
  const afterProfile = await profile(page)
  expect(afterProfile).toEqual(beforeProfile)
  expect(Number(sql('SELECT max(version) FROM preference_overlays'))).toBeGreaterThan(negativeOverlay)
  expect(Number(sql('SELECT max(ranking_version) FROM recommendation_runs WHERE published'))).toBeGreaterThan(dislikeRanking)
  expect(await (await page.request.get(`${api}/api/preference-adjustments?includeInactive=false`)).json()).toEqual([])
  expect(sql(`SELECT action FROM feedback_events WHERE recommendation_id='${target.id}' ORDER BY created_at DESC,id DESC LIMIT 1`)).toBe('like')

  const after = recommendations(await overview(page))
  const restored = after.find((item) => item.gameId === target.gameId)
  expect(restored?.latestFeedbackAction).toBe('like')
  const enteredNeighbor = after.find((item) => item.gameId === overlayNeighborID)
  expect(enteredNeighbor).toBeDefined()
  expect(enteredNeighbor!.factors.positivePreferenceSimilarity).toBeGreaterThan(0)
})

test('disabled OpenAI publishes a deterministic profile and missing vectors preserve direct effects', async ({ page, request }) => {
  test.skip(!openAIDisabled, 'disabled-provider deterministic fallback scenario')
  const user = await resetOwner(page, request)
  await startSync(page, user.csrfToken)
  const fallback = await profile(page)
  expect(fallback.summarySource).toBe('deterministic')
  expect(fallback.summary.trim()).not.toBe('')

  const target = recommendations(await overview(page))[0]
  expect(target).toBeDefined()
  const beforeRanking = Number(sql('SELECT max(ranking_version) FROM recommendation_runs WHERE published'))
  sql(`DELETE FROM game_embeddings WHERE game_id='${target.gameId}'`)
  await submit(page, user.csrfToken, target.id, 'like')
  const rebuilt = recommendations(await overview(page)).find((item) => item.gameId === target.gameId)
  expect(rebuilt?.latestFeedbackAction).toBe('like')
  expect(rebuilt?.factors.positiveDirectFeedback).toBe(1)
  expect(rebuilt?.weights.positiveDirectFeedback).toBe(20)
  expect(Number(sql('SELECT max(ranking_version) FROM recommendation_runs WHERE published'))).toBeGreaterThan(beforeRanking)
  expect(Number(sql(`SELECT coalesce((algorithm_metadata->>'missingVectors')::int,0) FROM preference_overlays ORDER BY version DESC LIMIT 1`))).toBeGreaterThan(0)
  expect((await profile(page)).summary).toBe(fallback.summary)
})

test('stale owner entry reuses one active refresh while demo entry is read-only', async ({ page, request, context }) => {
  const user = await resetOwner(page, request)
  await startSync(page, user.csrfToken)
  const saved = await profile(page)
  const savedOverview = await overview(page)
  const savedRecommendation = recommendations(savedOverview)[0]
  expect(savedRecommendation).toBeDefined()
  sql(`UPDATE sync_jobs SET updated_at=now()-interval '25 hours' WHERE kind='steam.sync' AND parent_job_id IS NULL AND status='succeeded'`)

  expect((await request.post(`${fake}/__test/steam-activity-mode?mode=delayed`)).status()).toBe(204)

  const refreshes: Array<{ status: number; body: { jobId?: string; reason: string } }> = []
  page.on('response', async (response) => {
    if (response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/sync/refresh-if-stale') {
      refreshes.push({ status: response.status(), body: await response.json() as { jobId?: string; reason: string } })
    }
  })
  await page.goto('/')
  await expect.poll(() => refreshes.length).toBeGreaterThan(0)
  expect(refreshes[0].status).toBe(202)
  expect(refreshes[0].body.reason).toBe('stale')
  const refreshJobID = refreshes[0].body.jobId
  expect(refreshJobID).toBeTruthy()
  expect((await profile(page)).id).toBe(saved.id)
  await expect(page.getByText(saved.summary)).toBeVisible()
  await expect(page.getByText(savedRecommendation.name).first()).toBeVisible()
  await expect.poll(async () => (await (await request.get(`${fake}/__test/catalog-counters`)).json() as { activityRequests: number }).activityRequests).toBeGreaterThan(0)

  const second = await context.newPage()
  const secondResponses: Array<{ jobId?: string; reason: string }> = []
  second.on('response', async (response) => {
    if (response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/sync/refresh-if-stale') secondResponses.push(await response.json() as { jobId?: string; reason: string })
  })
  await second.goto('/')
  await expect.poll(() => secondResponses.length).toBeGreaterThan(0)
  expect(secondResponses[0]).toMatchObject({ reason: 'active', jobId: refreshJobID })

  expect((await request.post(`${fake}/__test/steam-activity-mode?mode=healthy`)).status()).toBe(204)
  await expect.poll(async () => (await (await page.request.get(`${api}/api/sync/${refreshJobID}`)).json() as { status: string }).status, { timeout: 120_000 }).toBe('succeeded')

  const rootCount = sql(`SELECT count(*) FROM sync_jobs WHERE kind='steam.sync' AND parent_job_id IS NULL`)
  expect((await page.request.post(`${api}/auth/logout`, { headers: { 'X-CSRF-Token': user.csrfToken } })).status()).toBe(204)
  expect((await page.request.post(`${api}/auth/demo`, { headers: { Origin: web } })).status()).toBe(200)
  await page.goto('/')
  await expect(page.getByRole('status', { name: 'Демо-режим' })).toBeVisible()
  expect(sql(`SELECT count(*) FROM sync_jobs WHERE kind='steam.sync' AND parent_job_id IS NULL`)).toBe(rootCount)
})
