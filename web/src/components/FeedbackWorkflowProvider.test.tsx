import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { getRankingDiff } from '../api/client'
import FeedbackWorkflowProvider from './FeedbackWorkflowProvider'
import { useFeedbackWorkflows } from './FeedbackWorkflowContext'
import { overviewFixture } from '../test/overviewFixture'
import RecommendationCard from './RecommendationCard'

function Probe() {
  const workflow = useFeedbackWorkflows(); const state = workflow.stateFor('game-1')
  const send = (gameId: string, action: 'like' | 'dislike' | 'remind_me') => void workflow.submit({ gameId, recommendationId: `rec-${gameId}`, rankingVersion: 1, gameName: gameId, action, presentationOrigin: { surface: 'dashboard', lane: 'featured', recommendationId: `rec-${gameId}` } })
  return <div><button onClick={() => send('game-1', 'dislike')}>dislike one</button><button onClick={() => send('game-1', 'like')}>like one</button><button onClick={() => send('game-2', 'like')}>like two</button><span data-testid="phase">{state.phase}</span><span data-testid="action">{state.action ?? 'none'}</span><span data-testid="origin">{state.acceptedOrigin?.surface ?? 'none'}</span><span data-testid="busy">{String(state.busy)}</span><span data-testid="tombstone">{String(workflow.isTombstoned('game-1'))}</span></div>
}

const glowItem = { ...overviewFixture(1, 0).discoveries.featured!, gameId: 'game-1' }

const accepted = { eventId: 'event', rebuildJobId: 'job', previousRankingVersion: 1 }
const diff = { rebuildStatus: 'succeeded', previousVersion: 1, currentVersion: 2, added: [], removed: [], reordered: [] }

afterEach(() => vi.useRealTimers())

describe('FeedbackWorkflowProvider', () => {
  it('preserves one like effect deadline across recommendation remounts without replaying it', async () => {
    vi.useFakeTimers()
    const props = { csrfToken: 'csrf', submitFeedback: vi.fn().mockResolvedValue(accepted), readRankingDiff: vi.fn(() => new Promise<typeof diff>(() => undefined)), wait: async () => undefined, reload: async () => ({ kind: 'failed' as const, reason: 'request_failed' as const }) }
    const view = (recommendationId: string) => <FeedbackWorkflowProvider {...props}><RecommendationCard key={recommendationId} item={{ ...glowItem, id: recommendationId }} feedbackEnabled /></FeedbackWorkflowProvider>
    const rendered = render(view('rec-old'))

    fireEvent.click(screen.getByRole('button', { name: 'Нравится' }))
    fireEvent.click(screen.getByRole('button', { name: 'Сохранить отзыв' }))
    await act(async () => undefined)
    expect(screen.getByRole('article')).toHaveClass('recommendation-card--liked')
    expect(screen.getByRole('article')).toHaveStyle({ animationDelay: '0ms' })

    act(() => vi.advanceTimersByTime(300))
    rendered.rerender(view('rec-old'))
    expect(screen.getByRole('article')).toHaveStyle({ animationDelay: '0ms' })
    act(() => vi.advanceTimersByTime(200))
    rendered.rerender(view('rec-new'))
    expect(screen.getByRole('article')).toHaveClass('recommendation-card--liked')
    expect(screen.getByRole('article')).toHaveStyle({ animationDelay: '-500ms' })
    act(() => vi.advanceTimersByTime(219))
    expect(screen.getByRole('article')).toHaveClass('recommendation-card--liked')
    act(() => vi.advanceTimersByTime(1))
    expect(screen.getByRole('article')).not.toHaveClass('recommendation-card--liked')

    rendered.rerender(view('rec-after-consumption'))
    expect(screen.getByRole('article')).not.toHaveClass('recommendation-card--liked')
    rendered.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('accepts feedback, polls, and reloads after StrictMode replays effect setup', async () => {
    let finishPolling!: (value: typeof diff) => void
    const submitFeedback = vi.fn().mockResolvedValue(accepted)
    const readRankingDiff = vi.fn(() => new Promise<typeof diff>((resolve) => { finishPolling = resolve }))
    const onRankingUpdated = vi.fn()
    const reload = vi.fn().mockResolvedValue({ kind: 'fresh', data: { profile: null, overview: overviewFixture(0, 0) } })
    render(<StrictMode><FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} readRankingDiff={readRankingDiff} wait={async () => undefined} reload={reload} onRankingUpdated={onRankingUpdated}><Probe /></FeedbackWorkflowProvider></StrictMode>)

    fireEvent.click(screen.getByText('dislike one'))

    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('polling'))
		expect(screen.getByTestId('action')).toHaveTextContent('dislike')
		expect(screen.getByTestId('origin')).toHaveTextContent('dashboard')
    expect(screen.getByTestId('tombstone')).toHaveTextContent('true')
    expect(submitFeedback).toHaveBeenCalledTimes(1)
    expect(submitFeedback).toHaveBeenCalledWith('rec-game-1', 'csrf', { action: 'dislike', idempotencyKey: expect.any(String) }, expect.any(AbortSignal))
    expect(readRankingDiff).toHaveBeenCalledWith(1, 'job', expect.any(AbortSignal))
    expect(reload).not.toHaveBeenCalled()

    await act(async () => { finishPolling(diff) })

    await waitFor(() => expect(screen.getByTestId('busy')).toHaveTextContent('false'))
    expect(onRankingUpdated).toHaveBeenCalledExactlyOnceWith(diff)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(screen.getByTestId('tombstone')).toHaveTextContent('true')
    expect(screen.getByTestId('origin')).toHaveTextContent('dashboard')
  })

  it('aborts polling on real unmount and ignores its late result after StrictMode setup', async () => {
    let finishPolling!: (value: typeof diff) => void
    const readRankingDiff = vi.fn<typeof getRankingDiff>(() => new Promise<typeof diff>((resolve) => { finishPolling = resolve }))
    const onRankingUpdated = vi.fn()
    const reload = vi.fn()
    const { unmount } = render(<StrictMode><FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={vi.fn().mockResolvedValue(accepted)} readRankingDiff={readRankingDiff} wait={async () => undefined} reload={reload} onRankingUpdated={onRankingUpdated}><Probe /></FeedbackWorkflowProvider></StrictMode>)
    fireEvent.click(screen.getByText('dislike one'))
    await waitFor(() => expect(readRankingDiff).toHaveBeenCalledTimes(1))
    const signal = readRankingDiff.mock.calls[0][2]
    expect(signal?.aborted).toBe(false)

    unmount()
    expect(signal?.aborted).toBe(true)
    await act(async () => { finishPolling(diff) })

    expect(onRankingUpdated).not.toHaveBeenCalled()
    expect(reload).not.toHaveBeenCalled()
  })

  it('acquires synchronously per game and lets another game proceed', async () => {
    let resolve!: (value: typeof accepted) => void
    const submitFeedback = vi.fn(() => new Promise<typeof accepted>((done) => { resolve = done }))
    render(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} reload={async () => ({ kind: 'failed', reason: 'request_failed' })}><Probe /></FeedbackWorkflowProvider>)
    fireEvent.click(screen.getByText('dislike one')); fireEvent.click(screen.getByText('like one')); fireEvent.click(screen.getByText('like two'))
    expect(submitFeedback).toHaveBeenCalledTimes(2); resolve(accepted)
  })

  it('tombstones on acceptance, survives consumer loss, and retains retry after dead polling', async () => {
    let pollingSignal: AbortSignal | undefined
    const submitFeedback = vi.fn().mockResolvedValue(accepted)
    const readRankingDiff = vi.fn((_version, _job, signal) => { pollingSignal = signal; return Promise.resolve({ ...diff, rebuildStatus: 'dead' }) })
    const { rerender } = render(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} readRankingDiff={readRankingDiff} wait={async () => undefined} reload={async () => ({ kind: 'failed', reason: 'request_failed' })}><Probe /></FeedbackWorkflowProvider>)
    fireEvent.click(screen.getByText('dislike one'))
    await waitFor(() => expect(screen.getByTestId('tombstone')).toHaveTextContent('true'))
    rerender(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} readRankingDiff={readRankingDiff} wait={async () => undefined} reload={async () => ({ kind: 'failed', reason: 'request_failed' })}><div>initiator gone</div></FeedbackWorkflowProvider>)
    expect(pollingSignal?.aborted).toBe(false)
    expect(await screen.findByRole('alert')).toHaveTextContent('завершилась с ошибкой')
    fireEvent.click(screen.getByRole('button', { name: 'Повторить проверку' }))
    await waitFor(() => expect(readRankingDiff).toHaveBeenCalledTimes(2)); expect(submitFeedback).toHaveBeenCalledTimes(1)
  })

  it('keeps a dislike locked through stale reconciliation and releases only after fresh confirmation', async () => {
    const overview = overviewFixture(0, 0)
    const reload = vi.fn().mockResolvedValueOnce({ kind: 'stale', data: { profile: null, overview }, reason: 'request_failed' }).mockResolvedValueOnce({ kind: 'fresh', data: { profile: null, overview } })
    const submitFeedback = vi.fn().mockResolvedValue(accepted)
    render(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} readRankingDiff={vi.fn().mockResolvedValue(diff)} wait={async () => undefined} reload={reload}><Probe /></FeedbackWorkflowProvider>)
    fireEvent.click(screen.getByText('dislike one'))
    expect(await screen.findByRole('alert')).toHaveTextContent('Данные пока не подтверждены')
    expect(screen.getByTestId('busy')).toHaveTextContent('true')
    fireEvent.click(screen.getByRole('button', { name: 'Повторить проверку' }))
    await waitFor(() => expect(screen.getByTestId('busy')).toHaveTextContent('false'))
    expect(screen.getByTestId('tombstone')).toHaveTextContent('true'); expect(submitFeedback).toHaveBeenCalledTimes(1)
  })

  it('clears an existing tombstone only after a later fresh like confirmation', async () => {
    const dislikeOverview = overviewFixture(0, 0)
    const likeOverview = overviewFixture(1, 0); likeOverview.discoveries.featured!.gameId = 'game-1'; likeOverview.discoveries.featured!.latestFeedbackAction = 'like'
    const reload = vi.fn().mockResolvedValueOnce({ kind: 'fresh', data: { profile: null, overview: dislikeOverview } }).mockResolvedValueOnce({ kind: 'fresh', data: { profile: null, overview: likeOverview } })
    const submitFeedback = vi.fn().mockResolvedValue(accepted)
    render(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} readRankingDiff={vi.fn().mockResolvedValue(diff)} wait={async () => undefined} reload={reload}><Probe /></FeedbackWorkflowProvider>)
    fireEvent.click(screen.getByText('dislike one')); await waitFor(() => expect(screen.getByTestId('busy')).toHaveTextContent('false')); expect(screen.getByTestId('tombstone')).toHaveTextContent('true')
    fireEvent.click(screen.getByText('like one')); await waitFor(() => expect(screen.getByTestId('tombstone')).toHaveTextContent('false')); expect(submitFeedback).toHaveBeenCalledTimes(2)
  })

  it('keeps reminders neutral and aborts provider-owned work only on teardown', async () => {
    let signal: AbortSignal | undefined
    const submitFeedback = vi.fn((_id, _csrf, input, currentSignal) => { signal = currentSignal; expect(input).toEqual({ action: 'remind_me', idempotencyKey: expect.any(String) }); return new Promise<typeof accepted>(() => undefined) })
    function Reminder() { const workflow = useFeedbackWorkflows(); return <button onClick={() => void workflow.submit({ gameId: 'game-1', recommendationId: 'rec', rankingVersion: 1, gameName: 'Game', action: 'remind_me', reasons: ['genre'], freeText: 'secret' })}>remind</button> }
    const { unmount } = render(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} reload={async () => ({ kind: 'failed', reason: 'request_failed' })}><Reminder /></FeedbackWorkflowProvider>)
    fireEvent.click(screen.getByText('remind')); await waitFor(() => expect(signal).toBeDefined()); expect(signal?.aborted).toBe(false); unmount(); expect(signal?.aborted).toBe(true)
  })
})
