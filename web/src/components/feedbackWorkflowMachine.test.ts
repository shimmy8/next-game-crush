import { renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { DashboardReloadResult, FeedbackAccepted, RankingDiff } from '../api/types'
import { recommendation, overviewFixture } from '../test/overviewFixture'
import { useFeedbackWorkflows } from './FeedbackWorkflowContext'
import type { FeedbackSubmitIntent } from './FeedbackWorkflowContext'
import {
  canonicalFeedbackCommand,
  createFeedbackWorkflowRegistry,
  feedbackReasonOrder,
  feedbackWorkflowReducer,
  transitionFeedbackWorkflow,
} from './feedbackWorkflowMachine'
import type {
  CanonicalFeedbackCommand,
  FeedbackWorkflowRecord,
  FeedbackWorkflowState,
  FeedbackWorkflowTombstoneEffect,
} from './feedbackWorkflowMachine'

const intent: FeedbackSubmitIntent = {
  gameId: 'game-1',
  recommendationId: 'rec-1',
  rankingVersion: 1,
  gameName: 'Game',
  action: 'dislike',
  presentationOrigin: { surface: 'dashboard', lane: 'featured', recommendationId: 'rec-1' },
  reasons: ['other', 'genre'],
  freeText: ' note ',
}

const accepted: FeedbackAccepted = { eventId: 'event-1', rebuildJobId: 'job-1', previousRankingVersion: 1 }
const succeeded: RankingDiff = { rebuildStatus: 'succeeded', previousVersion: 1, currentVersion: 2, added: [], removed: [], reordered: [] }
const dead: RankingDiff = { ...succeeded, rebuildStatus: 'dead' }
function freshRecommendation(action: 'like' | 'dislike' | null, present = true): DashboardReloadResult {
  const overview = overviewFixture()
  if (present) {
    const item = recommendation('authoritative')
    item.gameId = intent.gameId
    item.latestFeedbackAction = action
    overview.discoveries.featured = item
  }
  return { kind: 'fresh', data: { profile: null, overview } }
}

const fresh = freshRecommendation('dislike')
const stale = { kind: 'stale', data: { profile: null, overview: overviewFixture() }, reason: 'request_failed' } satisfies DashboardReloadResult
const failed = { kind: 'failed', reason: 'request_failed' } satisfies DashboardReloadResult

function acquireRecord(
  overrides: Partial<FeedbackSubmitIntent> = {},
  command?: CanonicalFeedbackCommand,
  idempotencyKey = 'key-1',
): FeedbackWorkflowRecord {
  const nextIntent = { ...intent, ...overrides }
  return createFeedbackWorkflowRegistry().acquire(nextIntent, command ?? canonicalFeedbackCommand(nextIntent), idempotencyKey)!
}

function reduce(record: FeedbackWorkflowRecord, ...events: Parameters<typeof feedbackWorkflowReducer>[1][]) {
  return events.reduce<FeedbackWorkflowState>(feedbackWorkflowReducer, { [record.gameId]: record })
}

function applyTombstoneEffect(current: boolean, effect: FeedbackWorkflowTombstoneEffect): boolean {
  if (effect.disposition === 'retain') return true
  if (effect.disposition === 'clear') return false
  return current
}

describe('canonicalFeedbackCommand', () => {
  it('orders valid reasons by the existing taxonomy and removes absent optional fields', () => {
    expect(feedbackReasonOrder).toEqual([
      'genre', 'setting', 'multiplayer', 'single-player', 'difficulty', 'length',
      'early-access', 'already-known', 'not-now', 'other',
    ])
    expect(canonicalFeedbackCommand({ ...intent, reasons: ['other', 'genre', 'length'], freeText: '  ' }))
      .toEqual({ action: 'dislike', reasons: ['genre', 'length', 'other'] })
    expect(canonicalFeedbackCommand({ ...intent, reasons: [], freeText: undefined })).toEqual({ action: 'dislike' })
  })

  it.each([
    ['duplicate', ['genre', 'genre']],
    ['unknown', ['genre', 'surprise']],
    ['empty', ['genre', '']],
  ])('does not silently repair %s server-bound reasons', (_name, reasons) => {
    expect(canonicalFeedbackCommand({ ...intent, reasons })).toEqual({ action: 'dislike', reasons, freeText: 'note' })
  })

  it('strips reasons and free text from reminders', () => {
    expect(canonicalFeedbackCommand({ ...intent, action: 'remind_me' })).toEqual({ action: 'remind_me' })
  })
})

describe('feedback workflow registry', () => {
  it('acquires synchronously by stable game ID while other games stay independent', () => {
    const registry = createFeedbackWorkflowRegistry()
    const first = registry.acquire(intent, canonicalFeedbackCommand(intent), 'key-1')
    expect(first).not.toBeNull()
    expect(registry.acquire({ ...intent, recommendationId: 'new-snapshot' }, { action: 'like' }, 'key-2')).toBeNull()
    expect(registry.acquire({ ...intent, gameId: 'game-2' }, { action: 'like' }, 'key-3')).toMatchObject({ gameId: 'game-2', idempotencyKey: 'key-3' })
  })

  it('freezes the acquired command and idempotency identity', () => {
    const registry = createFeedbackWorkflowRegistry()
    const reasons = ['other', 'genre']
    const command: CanonicalFeedbackCommand = { action: 'dislike', reasons, freeText: 'note' }
    const record = registry.acquire(intent, command, 'key-1')!
    reasons.push('length')
    command.action = 'like'
    command.freeText = 'changed'
    expect(record.command).toEqual({ action: 'dislike', reasons: ['other', 'genre'], freeText: 'note' })
    expect(record.presentationOrigin).toEqual({ surface: 'dashboard', lane: 'featured', recommendationId: 'rec-1' })
    expect(record.idempotencyKey).toBe('key-1')
    expect(Object.isFrozen(record)).toBe(true)
    expect(Object.isFrozen(record.command)).toBe(true)
    expect(Object.isFrozen(record.command.reasons)).toBe(true)
    expect(Object.isFrozen(record.presentationOrigin)).toBe(true)
    expect(registry.get(intent.gameId)).toBe(record)
  })

  it('releases exactly once and treats duplicate release as a no-op', () => {
    const registry = createFeedbackWorkflowRegistry()
    registry.acquire(intent, canonicalFeedbackCommand(intent), 'key-1')
    expect(registry.release(intent.gameId)).toBe(true)
    expect(registry.release(intent.gameId)).toBe(false)
    expect(registry.get(intent.gameId)).toBeUndefined()
  })
})

describe('feedbackWorkflowReducer', () => {
  it('releases reducer state after a pre-acceptance failure only', () => {
    const record = acquireRecord()
    expect(reduce(record, { type: 'submit_failed', gameId: record.gameId })).toEqual({})
  })

  it('preserves an existing provider tombstone on pre-acceptance failure', () => {
    const record = acquireRecord({ action: 'like' }, { action: 'like' })
    const transition = transitionFeedbackWorkflow(
      { [record.gameId]: record },
      { type: 'submit_failed', gameId: record.gameId },
    )
    expect(transition.effect).toEqual({ type: 'released', gameId: record.gameId, tombstone: { disposition: 'unchanged' } })
    expect(applyTombstoneEffect(true, transition.effect!.tombstone)).toBe(true)
  })

  it('retains accepted identity, command, key, and tombstone through polling retries', () => {
    const record = acquireRecord()
    const unavailable = reduce(record,
      { type: 'accepted', gameId: record.gameId, accepted },
      { type: 'poll_failed', gameId: record.gameId },
    )[record.gameId]
    expect(unavailable).toMatchObject({ phase: 'polling_unavailable', accepted, command: record.command, idempotencyKey: 'key-1', tombstoned: true, error: 'polling_unavailable' })

    const polling = feedbackWorkflowReducer({ [record.gameId]: unavailable }, { type: 'retry_poll', gameId: record.gameId })[record.gameId]
    expect(polling).toMatchObject({ phase: 'polling', accepted, idempotencyKey: 'key-1', tombstoned: true, error: null })

    const rebuildDead = feedbackWorkflowReducer({ [record.gameId]: polling }, { type: 'poll_dead', gameId: record.gameId, diff: dead })[record.gameId]
    expect(rebuildDead).toMatchObject({ phase: 'rebuild_dead', accepted, diff: dead, tombstoned: true, error: 'rebuild_dead' })
    expect(Object.isFrozen(unavailable)).toBe(true)
    expect(Object.isFrozen(polling)).toBe(true)
    expect(Object.isFrozen(rebuildDead)).toBe(true)
    expect(unavailable.command).toBe(record.command)
    expect(polling.command).toBe(record.command)
    expect(rebuildDead.command).toBe(record.command)
  })

  it.each([stale, failed])('retains ownership and tombstone for $kind reconciliation', (result) => {
    const record = acquireRecord()
    const state = reduce(record,
      { type: 'accepted', gameId: record.gameId, accepted },
      { type: 'poll_succeeded', gameId: record.gameId, diff: succeeded },
      { type: 'reconcile', gameId: record.gameId, result, confirmedAction: null },
    )
    expect(state[record.gameId]).toMatchObject({ phase: 'stale_unconfirmed', accepted, diff: succeeded, tombstoned: true, error: 'stale_unconfirmed' })
    expect(feedbackWorkflowReducer(state, { type: 'retry_reconciliation', gameId: record.gameId })[record.gameId])
      .toMatchObject({ phase: 'reconciling', accepted, tombstoned: true, error: null })
  })

  it('does not release on a terminal diff without explicit fresh reconciliation', () => {
    const record = acquireRecord()
    const state = reduce(record,
      { type: 'accepted', gameId: record.gameId, accepted },
      { type: 'poll_succeeded', gameId: record.gameId, diff: succeeded },
    )
    expect(state[record.gameId]).toMatchObject({ phase: 'reconciling', accepted, diff: succeeded, tombstoned: true })
  })

  it.each([
    ['dislike', freshRecommendation(null, false), 'retain'],
    ['dislike', freshRecommendation('dislike'), 'retain'],
    ['like', freshRecommendation('like'), 'clear'],
    ['remind_me', freshRecommendation(null, false), 'unchanged'],
  ] as const)('releases a fresh confirmed %s with an explicit %s tombstone effect', (action, result, disposition) => {
    const record = acquireRecord({ action }, { action })
    const beforeReconciliation = reduce(record,
      { type: 'accepted', gameId: record.gameId, accepted },
      { type: 'poll_succeeded', gameId: record.gameId, diff: succeeded },
    )
    const transition = transitionFeedbackWorkflow(beforeReconciliation, {
      type: 'reconcile', gameId: record.gameId, result, confirmedAction: action === 'remind_me' ? null : action,
    })
    expect(transition.state).toEqual({})
    expect(transition.effect).toEqual({ type: 'released', gameId: record.gameId, tombstone: { disposition } })
  })

  it('keeps an existing provider tombstone unchanged after a registry-acquired reminder completes', () => {
    const record = acquireRecord({ action: 'remind_me' }, { action: 'remind_me' })
    expect(record.tombstoned).toBe(false)
    const reconciling = reduce(record,
      { type: 'accepted', gameId: record.gameId, accepted },
      { type: 'poll_succeeded', gameId: record.gameId, diff: succeeded },
    )
    const transition = transitionFeedbackWorkflow(reconciling, {
      type: 'reconcile', gameId: record.gameId, result: freshRecommendation(null, false),
    })
    expect(transition.effect).toEqual({ type: 'released', gameId: record.gameId, tombstone: { disposition: 'unchanged' } })
    expect(applyTombstoneEffect(true, transition.effect!.tombstone)).toBe(true)
  })

  it.each([
    ['like absent', 'like', freshRecommendation(null, false), 'like'],
    ['like still disliked', 'like', freshRecommendation('dislike'), 'like'],
    ['dislike still unreacted', 'dislike', freshRecommendation(null), 'dislike'],
    ['dislike now liked', 'dislike', freshRecommendation('like'), 'dislike'],
  ] as const)('keeps a fresh but contradictory %s locked without a terminal effect', (_name, action, result, claimedAction) => {
    const record = { ...acquireRecord({ action }, { action }), tombstoned: true }
    const state = reduce(record,
      { type: 'accepted', gameId: record.gameId, accepted },
      { type: 'poll_succeeded', gameId: record.gameId, diff: succeeded },
    )
    const transition = transitionFeedbackWorkflow(state, { type: 'reconcile', gameId: record.gameId, result, confirmedAction: claimedAction })
    expect(transition.state[record.gameId]).toMatchObject({ phase: 'stale_unconfirmed', tombstoned: true, error: 'stale_unconfirmed' })
    expect(transition.effect).toBeNull()
  })

  it.each([
    ['poll_succeeded', dead],
    ['poll_dead', succeeded],
  ] as const)('ignores %s when the diff status does not match the event', (type, diff) => {
    const record = acquireRecord()
    const polling = reduce(record, { type: 'accepted', gameId: record.gameId, accepted })
    expect(feedbackWorkflowReducer(polling, { type, gameId: record.gameId, diff })).toBe(polling)
  })

  it('tears down local reducer state without releasing accepted registry ownership', () => {
    const registry = createFeedbackWorkflowRegistry()
    const record = registry.acquire(intent, canonicalFeedbackCommand(intent), 'key-1')!
    const state = reduce(record, { type: 'accepted', gameId: record.gameId, accepted })
    expect(feedbackWorkflowReducer(state, { type: 'teardown' })).toEqual({})
    expect(registry.get(record.gameId)).toBe(record)
    expect(registry.release(record.gameId)).toBe(true)
  })

  it('ignores late or duplicate events after release', () => {
    expect(feedbackWorkflowReducer({}, { type: 'poll_failed', gameId: 'released-game' })).toEqual({})
  })

  it.each([
    ['accepted before acquisition completes', acquireRecord(), { type: 'poll_succeeded', gameId: intent.gameId, diff: succeeded }],
    ['reconciliation before terminal polling', { ...acquireRecord(), phase: 'polling', accepted }, { type: 'reconcile', gameId: intent.gameId, result: fresh, confirmedAction: 'dislike' }],
    ['poll retry before a retryable failure', { ...acquireRecord(), phase: 'polling', accepted }, { type: 'retry_poll', gameId: intent.gameId }],
    ['reconciliation retry before stale state', { ...acquireRecord(), phase: 'reconciling', accepted, diff: succeeded }, { type: 'retry_reconciliation', gameId: intent.gameId }],
  ] as const)('ignores out-of-order event: %s', (_name, record, event) => {
    const state = { [record.gameId]: record }
    expect(feedbackWorkflowReducer(state, event)).toBe(state)
  })

  it('does not overwrite an already acquired game in reducer state', () => {
    const record = acquireRecord()
    const competing = acquireRecord({ recommendationId: 'rec-2', action: 'like' }, { action: 'like' }, 'key-2')
    const state = { [record.gameId]: record }
    expect(feedbackWorkflowReducer(state, { type: 'acquired', record: competing })).toBe(state)
  })
})

describe('FeedbackWorkflowContext', () => {
  it('fails safely when a consumer is rendered outside the workflow provider', () => {
    expect(() => renderHook(() => useFeedbackWorkflows())).toThrow('FeedbackWorkflowProvider is required')
  })
})
