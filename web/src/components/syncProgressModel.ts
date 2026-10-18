import type { SyncStage, SyncStatus } from '../api/types'
import { stageLabel, statusLabel } from '../ui/labels'

export type SyncViewState = 'request-failed' | 'active' | 'failed' | 'partial' | 'complete' | 'unknown'

export type StageCounts = {
  total: number
  completed: number
  pending: number
  failed: number
  unknown: number
}

export type StageRow = {
  key: string
  label: string
  counts?: StageCounts
  statusLabel?: string
}

export type SyncPresentation = {
  state: SyncViewState
  headline: string
  warning?: string
  rows: StageRow[]
}

const ACTIVE_STATUSES = new Set(['queued', 'running', 'waiting', 'retryable'])
const COMPLETE_STATUSES = new Set(['succeeded', 'ready'])
const FAILED_STATUSES = new Set(['failed', 'dead'])
const REPEATED_STAGES = new Set(['achievements', 'catalog', 'ai_features', 'ai_embedding'])
const KNOWN_STAGES = new Set([
  'achievements',
  'ai_embedding',
  'ai_features',
  'catalog',
  'digest',
  'feedback',
  'owned_games',
  'profile',
  'profile_summary',
  'recently_played',
  'recommendations',
  'steam',
  'wishlist',
])

const STAGE_BY_KIND: Record<string, string> = {
  'achievements.sync': 'achievements',
  'ai.embedding': 'ai_embedding',
  'ai.features': 'ai_features',
  'ai.profile_summary': 'profile_summary',
  'catalog.refresh': 'catalog',
  'catalog.refresh_game': 'catalog',
  'feedback.classify': 'feedback',
  'notifications.digest.send': 'digest',
  'profile.rebuild': 'profile',
  'recommendations.rebuild': 'recommendations',
  'steam.catalog': 'catalog',
  'steam.sync': 'steam',
}

function stageKey(stage: SyncStage) {
  const unsuffixed = stage.stage.split(':', 1)[0]
  if (KNOWN_STAGES.has(unsuffixed)) return unsuffixed
  return STAGE_BY_KIND[stage.kind] ?? 'other'
}

function hasProblem(stage: SyncStage) {
  return stage.availability === 'unavailable'
    || stage.availability === 'request_failed'
    || FAILED_STATUSES.has(stage.status)
}

function countStage(counts: StageCounts, stage: SyncStage) {
  counts.total += 1
  if (stage.availability === 'request_failed' || FAILED_STATUSES.has(stage.status)) counts.failed += 1
  else if (COMPLETE_STATUSES.has(stage.status)) counts.completed += 1
  else if (ACTIVE_STATUSES.has(stage.status) || stage.status === 'pending' || stage.status === 'active') counts.pending += 1
  else counts.unknown += 1
}

function detailStatusLabel(stage: SyncStage) {
  if (stage.availability === 'unavailable') return statusLabel('unavailable')
  if (stage.availability === 'request_failed') return statusLabel('request_failed')
  if (stage.availability === 'not_modified') return statusLabel('not_modified')
  if (FAILED_STATUSES.has(stage.status) || ACTIVE_STATUSES.has(stage.status) || COMPLETE_STATUSES.has(stage.status)) {
    return statusLabel(stage.status)
  }
  if (stage.error) return 'Сбой этапа'
  return 'Статус уточняется'
}

export function groupSyncStages(stages: SyncStage[]): StageRow[] {
  const groups = new Map<string, SyncStage[]>()
  for (const stage of stages) {
    const key = stageKey(stage)
    const current = groups.get(key)
    if (current) current.push(stage)
    else groups.set(key, [stage])
  }

  return Array.from(groups, ([key, groupedStages]) => {
    const label = key === 'other' ? 'Другие этапы' : stageLabel(key)
    if (REPEATED_STAGES.has(key) || key === 'other' || groupedStages.length > 1) {
      const counts: StageCounts = { total: 0, completed: 0, pending: 0, failed: 0, unknown: 0 }
      for (const stage of groupedStages) countStage(counts, stage)
      const hasRetry = groupedStages.some((stage) => stage.status === 'retryable')
      return { key, label, counts, ...(hasRetry ? { statusLabel: 'Ожидают повтора' } : {}) }
    }
    return { key, label, statusLabel: detailStatusLabel(groupedStages[0]) }
  })
}

export function deriveSyncPresentation(value: SyncStatus | null, requestError?: string): SyncPresentation {
  const rows = groupSyncStages(value?.stages ?? [])
  const active = value ? ACTIVE_STATUSES.has(value.status) : false
  const hasUnavailableSource = value?.stages.some((stage) => stage.availability === 'unavailable') ?? false
  const hasFailedRequest = value?.stages.some((stage) => stage.availability === 'request_failed') ?? false
  const warning = active
    ? hasUnavailableSource
      ? 'Некоторые источники недоступны — продолжаем'
      : hasFailedRequest
        ? 'Не все данные удалось обновить — продолжаем'
        : undefined
    : undefined

  if (requestError) return { state: 'request-failed', headline: 'Не удалось обновить статус', warning, rows }
  if (active) return { state: 'active', headline: 'Синхронизация идёт', warning, rows }
  if (value && FAILED_STATUSES.has(value.status)) {
    return { state: 'failed', headline: 'Синхронизация остановилась с ошибкой', rows }
  }
  if (value && COMPLETE_STATUSES.has(value.status)) {
    const partial = value.stages.some(hasProblem)
    return partial
      ? { state: 'partial', headline: 'Синхронизация завершена не полностью', rows }
      : { state: 'complete', headline: 'Синхронизация завершена', rows }
  }
  return { state: 'unknown', headline: 'Статус уточняется', rows }
}
