import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { getRankingDiff, submitFeedback as apiSubmitFeedback } from '../api/client'
import type { DashboardReloadResult, FeedbackInput, RankingDiff } from '../api/types'
import { FeedbackWorkflowContext } from './FeedbackWorkflowContext'
import type { FeedbackPresentationOrigin, FeedbackSubmitIntent, FeedbackWorkflowContextValue } from './FeedbackWorkflowContext'
import { canonicalFeedbackCommand, createFeedbackWorkflowRegistry, feedbackWorkflowReducer } from './feedbackWorkflowMachine'
import type { FeedbackWorkflowEvent, FeedbackWorkflowRecord, FeedbackWorkflowState, PublicFeedbackWorkflowState } from './feedbackWorkflowMachine'
import { findRecommendationByGameId } from './feedbackVisibility'
import FeedbackWorkflowStatus, { feedbackErrorCopy } from './FeedbackWorkflowStatus'
import { pollRankingWorkflow, waitForRanking } from './rankingWorkflow'

export type FeedbackWorkflowProviderProps = {
  csrfToken: string; reload(): Promise<DashboardReloadResult>; onRankingUpdated?(diff: RankingDiff): void
  children: ReactNode; submitFeedback?: typeof apiSubmitFeedback; readRankingDiff?: typeof getRankingDiff
  wait?: typeof waitForRanking
}

const likeEffectMs = 720
const idle: PublicFeedbackWorkflowState = { phase: 'idle', accepted: false, acceptedOrigin: null, diff: null, error: '', canRetry: false, busy: false, tombstoned: false, action: null, likeEffectActive: false, likeEffectStartedAt: null, likeEffectAnimationDelayMs: 0 }

export default function FeedbackWorkflowProvider({ csrfToken, reload, onRankingUpdated, children, submitFeedback = apiSubmitFeedback, readRankingDiff = getRankingDiff, wait = waitForRanking }: FeedbackWorkflowProviderProps) {
  const [records, setRecords] = useState<FeedbackWorkflowState>({})
  const recordsRef = useRef<FeedbackWorkflowState>({})
  const registry = useRef(createFeedbackWorkflowRegistry())
  const controllers = useRef(new Map<string, AbortController>())
  const tombstones = useRef(new Set<string>())
  const tombstoneOrigins = useRef(new Map<string, FeedbackPresentationOrigin>())
  const likeEffectDeadlines = useRef(new Map<string, number>())
  const likeEffectTimers = useRef(new Map<string, number>())
  const [revision, setRevision] = useState(0)
  const mounted = useRef(true)
  const callbacks = useRef({ reload, onRankingUpdated })
  useEffect(() => { callbacks.current = { reload, onRankingUpdated } }, [reload, onRankingUpdated])

  const dispatch = useCallback((event: FeedbackWorkflowEvent) => {
    if (!mounted.current) return
    const next = feedbackWorkflowReducer(recordsRef.current, event)
    recordsRef.current = next; setRecords(next)
  }, [])
  const release = useCallback((gameId: string) => { registry.current.release(gameId); controllers.current.delete(gameId) }, [])

  const reconcile = useCallback(async (gameId: string) => {
    const record = recordsRef.current[gameId]
    if (!record) return
    const result = await callbacks.current.reload()
    if (!mounted.current || !recordsRef.current[gameId]) return
    let confirmedAction = null
    if (result.kind === 'fresh') confirmedAction = findRecommendationByGameId(result.data.overview, gameId)?.latestFeedbackAction ?? null
    const confirmed = result.kind === 'fresh' && (record.command.action === 'remind_me'
      || (record.command.action === 'dislike' && (confirmedAction === 'dislike' || !findRecommendationByGameId(result.data.overview, gameId)))
      || (record.command.action === 'like' && confirmedAction === 'like'))
    dispatch({ type: 'reconcile', gameId, result: confirmed ? result : result.kind === 'fresh' ? { kind: 'stale', data: result.data, reason: 'request_failed' } : result, confirmedAction })
    if (confirmed) {
      if (record.command.action === 'like') { tombstones.current.delete(gameId); tombstoneOrigins.current.delete(gameId) }
      release(gameId); setRevision((value) => value + 1)
    }
  }, [dispatch, release])

  const poll = useCallback(async (gameId: string, record: FeedbackWorkflowRecord, controller: AbortController) => {
    if (!record.accepted) return
    try {
      const diff = await pollRankingWorkflow(record.accepted, controller.signal, readRankingDiff, wait)
      if (controller.signal.aborted || !mounted.current) return
      if (diff.rebuildStatus === 'dead') { dispatch({ type: 'poll_dead', gameId, diff }); return }
      dispatch({ type: 'poll_succeeded', gameId, diff })
      callbacks.current.onRankingUpdated?.(diff)
      await reconcile(gameId)
    } catch {
      if (!controller.signal.aborted) dispatch({ type: 'poll_failed', gameId })
    }
  }, [dispatch, readRankingDiff, reconcile, wait])

  const submit = useCallback(async (intent: FeedbackSubmitIntent) => {
    const command = canonicalFeedbackCommand(intent)
    const record = registry.current.acquire(intent, command, crypto.randomUUID())
    if (!record) return
    dispatch({ type: 'acquired', record })
    const controller = new AbortController(); controllers.current.set(intent.gameId, controller)
    const input: FeedbackInput = { ...command, idempotencyKey: record.idempotencyKey }
    try {
      const accepted = await submitFeedback(intent.recommendationId, csrfToken, input, controller.signal)
      if (controller.signal.aborted || !mounted.current) return
      const acceptedRecord = { ...record, accepted, phase: 'polling' as const, tombstoned: command.action === 'dislike' }
      if (command.action === 'dislike') {
        tombstones.current.add(intent.gameId)
        if (acceptedRecord.presentationOrigin) tombstoneOrigins.current.set(intent.gameId, acceptedRecord.presentationOrigin)
        else tombstoneOrigins.current.delete(intent.gameId)
      }
      if (command.action === 'like') {
        const previousTimer = likeEffectTimers.current.get(intent.gameId)
        if (previousTimer !== undefined) window.clearTimeout(previousTimer)
        likeEffectDeadlines.current.set(intent.gameId, Date.now() + likeEffectMs)
        likeEffectTimers.current.set(intent.gameId, window.setTimeout(() => {
          likeEffectTimers.current.delete(intent.gameId)
          likeEffectDeadlines.current.delete(intent.gameId)
          if (mounted.current) setRevision((value) => value + 1)
        }, likeEffectMs))
      }
      dispatch({ type: 'accepted', gameId: intent.gameId, accepted }); setRevision((value) => value + 1)
      void poll(intent.gameId, acceptedRecord, controller)
    } catch {
      if (!controller.signal.aborted) { dispatch({ type: 'submit_failed', gameId: intent.gameId }); release(intent.gameId) }
    }
  }, [csrfToken, dispatch, poll, release, submitFeedback])

  const retry = useCallback((gameId: string) => {
    const record = recordsRef.current[gameId]
    if (!record) return
    if (record.phase === 'stale_unconfirmed') { dispatch({ type: 'retry_reconciliation', gameId }); void reconcile(gameId); return }
    if ((record.phase === 'polling_unavailable' || record.phase === 'rebuild_dead') && record.accepted) {
      const old = controllers.current.get(gameId); old?.abort()
      const controller = new AbortController(); controllers.current.set(gameId, controller)
      dispatch({ type: 'retry_poll', gameId }); void poll(gameId, { ...record, phase: 'polling' }, controller)
    }
  }, [dispatch, poll, reconcile])

  useEffect(() => {
    mounted.current = true
    const activeControllers = controllers.current
    const activeLikeTimers = likeEffectTimers.current
    const activeLikeDeadlines = likeEffectDeadlines.current
    return () => { mounted.current = false; activeControllers.forEach((controller) => controller.abort()); activeLikeTimers.forEach((timer) => window.clearTimeout(timer)); activeLikeTimers.clear(); activeLikeDeadlines.clear(); recordsRef.current = feedbackWorkflowReducer(recordsRef.current, { type: 'teardown' }) }
  }, [])

  const stateFor = useCallback((gameId: string): PublicFeedbackWorkflowState => {
    void revision
    const record = records[gameId]
    const now = Date.now()
    const likeEffectDeadline = likeEffectDeadlines.current.get(gameId) ?? 0
    const likeEffectActive = likeEffectDeadline > now
    const likeEffectStartedAt = likeEffectActive ? likeEffectDeadline - likeEffectMs : null
    const likeEffectAnimationDelayMs = likeEffectStartedAt === null ? 0 : -Math.max(0, now - likeEffectStartedAt)
    const acceptedOrigin = record?.accepted ? record.presentationOrigin : tombstoneOrigins.current.get(gameId) ?? null
    if (!record) return tombstones.current.has(gameId) ? { ...idle, acceptedOrigin, tombstoned: true, action: 'dislike', likeEffectActive, likeEffectStartedAt, likeEffectAnimationDelayMs } : { ...idle, likeEffectActive, likeEffectStartedAt, likeEffectAnimationDelayMs }
    return { phase: record.phase, accepted: Boolean(record.accepted), acceptedOrigin, diff: record.diff, error: record.error ? feedbackErrorCopy(record.error) : '', canRetry: record.phase === 'polling_unavailable' || record.phase === 'rebuild_dead' || record.phase === 'stale_unconfirmed', busy: true, tombstoned: tombstones.current.has(gameId) || record.tombstoned, action: record.accepted ? record.command.action : null, likeEffectActive, likeEffectStartedAt, likeEffectAnimationDelayMs }
  }, [records, revision])
  const value = useMemo<FeedbackWorkflowContextValue>(() => ({ stateFor, submit, retry, isBusy: (gameId) => Boolean(records[gameId]), isTombstoned: (gameId) => tombstones.current.has(gameId) }), [records, retry, stateFor, submit])
  const statusIds = Object.keys(records)
  return <FeedbackWorkflowContext.Provider value={value}>{children}<FeedbackWorkflowStatus gameIds={statusIds} /></FeedbackWorkflowContext.Provider>
}
