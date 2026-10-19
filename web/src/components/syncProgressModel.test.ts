import { describe, expect, it } from 'vitest'
import type { SyncStage, SyncStatus } from '../api/types'
import { deriveSyncPresentation, groupSyncStages } from './syncProgressModel'

const status = (rootStatus: string, stages: SyncStage[] = []): SyncStatus => ({
  id: 'job', stage: 'steam', status: rootStatus, progress: 50, stages,
})

describe('deriveSyncPresentation', () => {
  it('keeps an unavailable optional source secondary while the workflow is active', () => {
    const result = deriveSyncPresentation(status('running', [
      { kind: 'steam.source', stage: 'wishlist', status: 'degraded', availability: 'unavailable', error: 'private' },
    ]))
    expect(result).toMatchObject({ state: 'active', headline: 'Синхронизация идёт', warning: 'Некоторые источники недоступны — продолжаем' })
  })

  it('distinguishes a failed source request while the workflow is active', () => {
    const result = deriveSyncPresentation(status('running', [
      { kind: 'steam.source', stage: 'owned_games', status: 'degraded', availability: 'request_failed' },
    ]))
    expect(result).toMatchObject({ state: 'active', headline: 'Синхронизация идёт', warning: 'Не все данные удалось обновить — продолжаем' })
  })

  it('prioritizes unavailable-source copy when active sources have mixed failures', () => {
    const result = deriveSyncPresentation(status('running', [
      { kind: 'steam.source', stage: 'wishlist', status: 'degraded', availability: 'request_failed' },
      { kind: 'steam.source', stage: 'owned_games', status: 'degraded', availability: 'unavailable' },
    ]))
    expect(result.warning).toBe('Некоторые источники недоступны — продолжаем')
  })

  it('omits source warnings for a healthy active workflow', () => {
    const result = deriveSyncPresentation(status('running', [
      { kind: 'steam.source', stage: 'wishlist', status: 'ready', availability: 'available' },
      { kind: 'steam.catalog', stage: 'catalog', status: 'running' },
    ]))
    expect(result).toMatchObject({ state: 'active', headline: 'Синхронизация идёт' })
    expect(result.warning).toBeUndefined()
  })

  it('keeps retryable work in stage detail under the active headline', () => {
    const result = deriveSyncPresentation(status('waiting', [
      { kind: 'catalog.refresh', stage: 'catalog', status: 'retryable', error: 'temporary upstream timeout' },
    ]))
    expect(result.headline).toBe('Синхронизация идёт')
    expect(result.rows).toContainEqual(expect.objectContaining({ statusLabel: 'Ожидают повтора' }))
  })

  it('uses the terminal headline for a failed root workflow', () => {
    expect(deriveSyncPresentation(status('dead')).headline).toBe('Синхронизация остановилась с ошибкой')
  })

  it.each([
    { kind: 'steam.source', stage: 'wishlist', status: 'degraded', availability: 'unavailable' },
    { kind: 'steam.source', stage: 'owned_games', status: 'degraded', availability: 'request_failed' },
    { kind: 'ai.profile_summary', stage: 'profile_summary', status: 'failed', error: 'upstream request failed' },
  ])('marks a successful workflow with an optional problem as partial', (stage) => {
    expect(deriveSyncPresentation(status('succeeded', [stage])).headline).toBe('Синхронизация завершена не полностью')
  })

  it('uses the complete headline for a clean successful workflow', () => {
    expect(deriveSyncPresentation(status('ready', [
      { kind: 'steam.catalog', stage: 'catalog:620', status: 'ready', availability: 'not_modified' },
    ])).headline).toBe('Синхронизация завершена')
  })

  it('gives a status request failure precedence while retaining snapshot rows', () => {
    const result = deriveSyncPresentation(status('running', [
      { kind: 'steam.source', stage: 'wishlist', status: 'ready', availability: 'available' },
    ]), 'network error')
    expect(result).toMatchObject({ state: 'request-failed', headline: 'Не удалось обновить статус' })
    expect(result.rows).toHaveLength(1)
  })

  it('renders unknown root and child states safely', () => {
    const result = deriveSyncPresentation(status('future', [
      { kind: 'steam.source', stage: 'wishlist', status: 'future' },
    ]))
    expect(result).toMatchObject({ state: 'unknown', headline: 'Статус уточняется' })
    expect(result.rows).toContainEqual(expect.objectContaining({ statusLabel: 'Статус уточняется' }))
  })
})

describe('groupSyncStages', () => {
  it('groups preference overlay work under its safe localized stage', () => {
    expect(groupSyncStages([
      { kind: 'preferences.rebuild', stage: 'preferences', status: 'running' },
    ])).toEqual([expect.objectContaining({ key: 'preferences', label: 'Предпочтения' })])
  })

  it.each(['queued', 'running', 'waiting', 'retryable'])('counts %s work with a retained error as pending', (stageStatus) => {
    const rows = groupSyncStages([
      { kind: 'ai.features', stage: 'ai_features', status: stageStatus, error: 'temporary upstream timeout' },
    ])

    expect(rows).toEqual([
      expect.objectContaining({
        key: 'ai_features',
        counts: { total: 1, completed: 0, pending: 1, failed: 0, unknown: 0 },
      }),
    ])
  })

  it('counts only terminal failures and failed source requests as failed', () => {
    const rows = groupSyncStages([
      { kind: 'ai.features', stage: 'ai_features', status: 'dead', error: 'attempts exhausted' },
      { kind: 'ai.features', stage: 'ai_features', status: 'failed', error: 'terminal failure' },
      { kind: 'ai.features', stage: 'ai_features', status: 'succeeded', error: 'retained previous error' },
      { kind: 'steam.source', stage: 'achievements:10', status: 'degraded', availability: 'request_failed', error: 'upstream request failed' },
    ])

    expect(rows).toEqual([
      expect.objectContaining({
        key: 'ai_features',
        counts: { total: 3, completed: 1, pending: 0, failed: 2, unknown: 0 },
      }),
      expect.objectContaining({
        key: 'achievements',
        counts: { total: 1, completed: 0, pending: 0, failed: 1, unknown: 0 },
      }),
    ])
  })

  it('groups repeated per-game and AI work into safe counts', () => {
    const appIDs = ['620', '440']
    const rows = groupSyncStages([
      { kind: 'steam.source', stage: `achievements:${appIDs[0]}`, status: 'ready', availability: 'available' },
      { kind: 'steam.source', stage: `achievements:${appIDs[1]}`, status: 'degraded', availability: 'unavailable' },
      { kind: 'steam.catalog', stage: `catalog:${appIDs[0]}`, status: 'ready', availability: 'not_modified' },
      { kind: 'steam.catalog', stage: `catalog:${appIDs[1]}`, status: 'retryable' },
      { kind: 'ai.features', stage: 'ai_features', status: 'succeeded' },
      { kind: 'ai.features', stage: 'ai_features', status: 'waiting' },
      { kind: 'ai.embedding', stage: 'ai_embedding', status: 'dead' },
      { kind: 'ai.embedding', stage: 'ai_embedding', status: 'future' },
    ])
    expect(rows).toEqual(expect.arrayContaining([
      expect.objectContaining({ key: 'achievements', label: 'Достижения', counts: { total: 2, completed: 1, pending: 0, failed: 0, unknown: 1 } }),
      expect.objectContaining({ key: 'catalog', label: 'Каталог Steam', counts: { total: 2, completed: 1, pending: 1, failed: 0, unknown: 0 } }),
      expect.objectContaining({ key: 'ai_features', label: 'AI-признаки', counts: { total: 2, completed: 1, pending: 1, failed: 0, unknown: 0 } }),
      expect.objectContaining({ key: 'ai_embedding', label: 'AI-вектор', counts: { total: 2, completed: 0, pending: 0, failed: 1, unknown: 1 } }),
    ]))
    expect(JSON.stringify(rows)).not.toContain(appIDs[0])
    expect(JSON.stringify(rows)).not.toContain(appIDs[1])
  })

  it('sanitizes even a single suffixed stage', () => {
    const rows = groupSyncStages([{ kind: 'steam.catalog', stage: 'catalog:730', status: 'ready', availability: 'available' }])
    expect(rows).toEqual([expect.objectContaining({ key: 'catalog', label: 'Каталог Steam', counts: expect.objectContaining({ total: 1 }) })])
    expect(JSON.stringify(rows)).not.toContain('730')
  })

  it('groups unknown future stages under a generic row', () => {
    const rows = groupSyncStages([
      { kind: 'future.one', stage: 'future:secret-one', status: 'running' },
      { kind: 'future.two', stage: 'different:secret-two', status: 'future' },
    ])
    expect(rows).toEqual([{ key: 'other', label: 'Другие этапы', counts: { total: 2, completed: 0, pending: 1, failed: 0, unknown: 1 } }])
    expect(JSON.stringify(rows)).not.toMatch(/secret-one|secret-two/)
  })

  it('uses one stable row identity for duplicate kind and stage rows', () => {
    const rows = groupSyncStages([
      { kind: 'profile.rebuild', stage: 'profile', status: 'running' },
      { kind: 'profile.rebuild', stage: 'profile', status: 'succeeded' },
    ])
    expect(rows).toHaveLength(1)
    expect(new Set(rows.map((row) => row.key)).size).toBe(rows.length)
    expect(rows[0]).toMatchObject({ key: 'profile', counts: { total: 2, completed: 1, pending: 1, failed: 0, unknown: 0 } })
  })
})
