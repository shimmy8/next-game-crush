import { createContext, useContext } from 'react'
import type { FeedbackAction } from '../api/types'
import type { PublicFeedbackWorkflowState } from './feedbackWorkflowMachine'

export type FeedbackPresentationOrigin =
  | { surface: 'dashboard'; lane: 'featured' | 'discoveries' | 'wishlist'; recommendationId: string }
  | { surface: 'calendar'; recommendationId: string }

export type FeedbackSubmitIntent = {
  gameId: string
  recommendationId: string
  rankingVersion: number
  gameName: string
  action: FeedbackAction
  presentationOrigin?: FeedbackPresentationOrigin
  reasons?: string[]
  freeText?: string
}

export type FeedbackWorkflowContextValue = {
  stateFor(gameId: string): PublicFeedbackWorkflowState
  submit(input: FeedbackSubmitIntent): Promise<void>
  retry(gameId: string): void
  isBusy(gameId: string): boolean
  isTombstoned(gameId: string): boolean
}

const idle: PublicFeedbackWorkflowState = { phase: 'idle', accepted: false, acceptedOrigin: null, diff: null, error: '', canRetry: false, busy: false, tombstoned: false, action: null, likeEffectActive: false, likeEffectStartedAt: null, likeEffectAnimationDelayMs: 0 }
const legacyConsumerFallback: FeedbackWorkflowContextValue = { stateFor: () => idle, submit: async () => undefined, retry: () => undefined, isBusy: () => false, isTombstoned: () => false }
export const FeedbackWorkflowContext = createContext<FeedbackWorkflowContextValue | undefined>(undefined)

export function useFeedbackWorkflows(): FeedbackWorkflowContextValue {
  const value = useContext(FeedbackWorkflowContext)
  if (!value) throw new Error('FeedbackWorkflowProvider is required')
  return value
}

export function useGameFeedback(gameId: string): PublicFeedbackWorkflowState & {
  submit(input: Omit<FeedbackSubmitIntent, 'gameId'>): Promise<void>
  retry(): void
} {
  // Task 6 migrates every product consumer under the page provider. Keep this
  // helper compatible until then while direct context access fails closed.
  const workflows = useContext(FeedbackWorkflowContext) ?? legacyConsumerFallback
  return {
    ...workflows.stateFor(gameId),
    submit: (input: Omit<FeedbackSubmitIntent, 'gameId'>) => workflows.submit({ ...input, gameId }),
    retry: () => workflows.retry(gameId),
  }
}
