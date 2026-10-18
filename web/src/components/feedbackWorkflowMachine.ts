import type { DashboardReloadResult, FeedbackAccepted, FeedbackAction, FeedbackInput, RankingDiff } from '../api/types'
import type { FeedbackPresentationOrigin, FeedbackSubmitIntent } from './FeedbackWorkflowContext'

export const feedbackReasonOrder = ['genre', 'setting', 'multiplayer', 'single-player', 'difficulty', 'length', 'early-access', 'already-known', 'not-now', 'other'] as const
export type FeedbackWorkflowPhase = 'submitting' | 'polling' | 'polling_unavailable' | 'rebuild_dead' | 'reconciling' | 'stale_unconfirmed'
export type FeedbackWorkflowError = 'submit_failed' | 'polling_unavailable' | 'rebuild_dead' | 'stale_unconfirmed'
export type CanonicalFeedbackCommand = Pick<FeedbackInput, 'action' | 'reasons' | 'freeText'>
export type FeedbackWorkflowRecord = {
  gameId: string; sourceRecommendationId: string; sourceRankingVersion: number; gameName: string
  command: CanonicalFeedbackCommand; idempotencyKey: string; phase: FeedbackWorkflowPhase
  accepted: FeedbackAccepted | null; presentationOrigin: FeedbackPresentationOrigin | null; diff: RankingDiff | null; error: FeedbackWorkflowError | null; tombstoned: boolean
}
export type FeedbackWorkflowState = Record<string, FeedbackWorkflowRecord>
export type PublicFeedbackWorkflowState = {
  phase: FeedbackWorkflowPhase | 'idle'; accepted: boolean; acceptedOrigin: FeedbackPresentationOrigin | null; diff: RankingDiff | null; error: string
  canRetry: boolean; busy: boolean; tombstoned: boolean; action: FeedbackAction | null; likeEffectActive: boolean; likeEffectStartedAt: number | null; likeEffectAnimationDelayMs: number
}
export type FeedbackWorkflowEvent =
  | { type: 'acquired'; record: FeedbackWorkflowRecord }
  | { type: 'submit_failed'; gameId: string }
  | { type: 'accepted'; gameId: string; accepted: FeedbackAccepted }
  | { type: 'poll_failed'; gameId: string }
  | { type: 'poll_dead'; gameId: string; diff: RankingDiff }
  | { type: 'retry_poll'; gameId: string }
  | { type: 'poll_succeeded'; gameId: string; diff: RankingDiff }
  | { type: 'reconcile'; gameId: string; result: DashboardReloadResult; /** @deprecated Ignored; fresh confirmation is derived from result.data. */ confirmedAction?: FeedbackAction | null }
  | { type: 'retry_reconciliation'; gameId: string }
  | { type: 'teardown' }
export type FeedbackWorkflowTombstoneEffect =
  | { disposition: 'retain' }
  | { disposition: 'clear' }
  | { disposition: 'unchanged' }
export type FeedbackWorkflowEffect = {
  type: 'released'
  gameId: string
  tombstone: FeedbackWorkflowTombstoneEffect
}
export type FeedbackWorkflowTransitionResult = {
  state: FeedbackWorkflowState
  effect: FeedbackWorkflowEffect | null
}

export function canonicalFeedbackCommand(intent: FeedbackSubmitIntent): CanonicalFeedbackCommand {
  if (intent.action === 'remind_me') return { action: 'remind_me' }
  const reasons = intent.reasons ? [...intent.reasons] : undefined
  const validReasons = reasons
    && reasons.every((reason) => (feedbackReasonOrder as readonly string[]).includes(reason))
    && new Set(reasons).size === reasons.length
  if (validReasons) {
    reasons.sort((left, right) => feedbackReasonOrder.indexOf(left as typeof feedbackReasonOrder[number]) - feedbackReasonOrder.indexOf(right as typeof feedbackReasonOrder[number]))
  }
  return {
    action: intent.action,
    ...(reasons?.length ? { reasons } : {}),
    ...(intent.freeText?.trim() ? { freeText: intent.freeText.trim() } : {}),
  }
}

export type FeedbackWorkflowRegistry = {
  acquire(intent: FeedbackSubmitIntent, command: CanonicalFeedbackCommand, idempotencyKey: string): FeedbackWorkflowRecord | null
  get(gameId: string): FeedbackWorkflowRecord | undefined
  release(gameId: string): boolean
}

function freezeFeedbackCommand(command: CanonicalFeedbackCommand): CanonicalFeedbackCommand {
  if (Object.isFrozen(command) && (!command.reasons || Object.isFrozen(command.reasons))) return command
  const reasons = command.reasons ? Object.freeze([...command.reasons]) as string[] : undefined
  return Object.freeze<CanonicalFeedbackCommand>({
    action: command.action,
    ...(reasons ? { reasons } : {}),
    ...(command.freeText !== undefined ? { freeText: command.freeText } : {}),
  })
}

function freezeFeedbackRecord(record: FeedbackWorkflowRecord): FeedbackWorkflowRecord {
  return Object.freeze<FeedbackWorkflowRecord>({ ...record, command: freezeFeedbackCommand(record.command) })
}

function replaceRecord(state: FeedbackWorkflowState, record: FeedbackWorkflowRecord): FeedbackWorkflowState {
  return { ...state, [record.gameId]: freezeFeedbackRecord(record) }
}

function unchanged(state: FeedbackWorkflowState): FeedbackWorkflowTransitionResult {
  return { state, effect: null }
}

function authoritativeFeedback(result: Extract<DashboardReloadResult, { kind: 'fresh' }>, gameId: string) {
  const { overview } = result.data
  const recommendations = [
    ...(overview.discoveries.featured ? [overview.discoveries.featured] : []),
    ...overview.discoveries.items,
    ...overview.wishlist.items,
    ...overview.calendar.exactDates,
    ...overview.calendar.month,
    ...overview.calendar.quarter,
    ...overview.calendar.year,
    ...overview.calendar.comingSoon,
    ...overview.calendar.tba,
  ]
  const recommendation = recommendations.find((item) => item.gameId === gameId)
  return recommendation
    ? { present: true, action: recommendation.latestFeedbackAction }
    : { present: false, action: null }
}

export function createFeedbackWorkflowRegistry(): FeedbackWorkflowRegistry {
  const records = new Map<string, FeedbackWorkflowRecord>()
  return {
    acquire(intent, command, idempotencyKey) {
      if (records.has(intent.gameId)) return null
      const presentationOrigin = intent.presentationOrigin ? Object.freeze({ ...intent.presentationOrigin, recommendationId: intent.recommendationId }) : null
      const record = freezeFeedbackRecord({ gameId: intent.gameId, sourceRecommendationId: intent.recommendationId, sourceRankingVersion: intent.rankingVersion, gameName: intent.gameName, command, idempotencyKey, phase: 'submitting', accepted: null, presentationOrigin, diff: null, error: null, tombstoned: false })
      records.set(intent.gameId, record)
      return record
    },
    get: (gameId) => records.get(gameId),
    release: (gameId) => records.delete(gameId),
  }
}

export function transitionFeedbackWorkflow(state: FeedbackWorkflowState, event: FeedbackWorkflowEvent): FeedbackWorkflowTransitionResult {
  switch (event.type) {
    case 'teardown':
      return { state: {}, effect: null }
    case 'acquired':
      if (state[event.record.gameId]) return unchanged(state)
      return { state: replaceRecord(state, event.record), effect: null }
    case 'submit_failed': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'submitting' || current.accepted) return unchanged(state)
      const next = { ...state }
      delete next[event.gameId]
      return { state: next, effect: { type: 'released', gameId: event.gameId, tombstone: { disposition: 'unchanged' } } }
    }
    case 'accepted': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'submitting' || current.accepted) return unchanged(state)
      return { state: replaceRecord(state, { ...current, phase: 'polling', accepted: event.accepted, tombstoned: current.command.action === 'dislike' || current.tombstoned }), effect: null }
    }
    case 'poll_failed': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'polling' || !current.accepted) return unchanged(state)
      return { state: replaceRecord(state, { ...current, phase: 'polling_unavailable', error: 'polling_unavailable' }), effect: null }
    }
    case 'poll_dead': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'polling' || !current.accepted || event.diff.rebuildStatus !== 'dead') return unchanged(state)
      return { state: replaceRecord(state, { ...current, phase: 'rebuild_dead', diff: event.diff, error: 'rebuild_dead' }), effect: null }
    }
    case 'retry_poll': {
      const current = state[event.gameId]
      if (!current || !current.accepted || (current.phase !== 'polling_unavailable' && current.phase !== 'rebuild_dead')) return unchanged(state)
      return { state: replaceRecord(state, { ...current, phase: 'polling', error: null }), effect: null }
    }
    case 'poll_succeeded': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'polling' || !current.accepted || event.diff.rebuildStatus !== 'succeeded') return unchanged(state)
      return { state: replaceRecord(state, { ...current, phase: 'reconciling', diff: event.diff, error: null }), effect: null }
    }
    case 'retry_reconciliation': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'stale_unconfirmed' || !current.accepted) return unchanged(state)
      return { state: replaceRecord(state, { ...current, phase: 'reconciling', error: null }), effect: null }
    }
    case 'reconcile': {
      const current = state[event.gameId]
      if (!current || current.phase !== 'reconciling' || !current.accepted || !current.diff) return unchanged(state)
      if (event.result.kind !== 'fresh') return { state: replaceRecord(state, { ...current, phase: 'stale_unconfirmed', error: 'stale_unconfirmed' }), effect: null }
      const authoritative = authoritativeFeedback(event.result, event.gameId)
      const confirmed = current.command.action === 'remind_me'
        || (current.command.action === 'dislike' && (!authoritative.present || authoritative.action === 'dislike'))
        || (current.command.action === 'like' && authoritative.present && authoritative.action === 'like')
      if (!confirmed) return { state: replaceRecord(state, { ...current, phase: 'stale_unconfirmed', error: 'stale_unconfirmed' }), effect: null }
      const next = { ...state }
      delete next[event.gameId]
      const tombstone = current.command.action === 'dislike'
        ? { disposition: 'retain' as const }
        : current.command.action === 'like'
          ? { disposition: 'clear' as const }
          : { disposition: 'unchanged' as const }
      return { state: next, effect: { type: 'released', gameId: event.gameId, tombstone } }
    }
  }
}

export function feedbackWorkflowReducer(state: FeedbackWorkflowState, event: FeedbackWorkflowEvent): FeedbackWorkflowState {
  return transitionFeedbackWorkflow(state, event).state
}
