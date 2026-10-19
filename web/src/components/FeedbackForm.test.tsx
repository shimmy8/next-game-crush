import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import FeedbackForm from './FeedbackForm'
import FeedbackWorkflowProvider from './FeedbackWorkflowProvider'
import { overviewFixture } from '../test/overviewFixture'

function renderForm(options: { readOnly?: boolean; initialAction?: 'like' | 'dislike'; initialReasons?: string[] } = {}) {
  const submitFeedback = vi.fn().mockResolvedValue({ eventId: 'event', rebuildJobId: 'job', previousRankingVersion: 1 })
  const readRankingDiff = vi.fn().mockResolvedValue({ rebuildStatus: 'succeeded', previousVersion: 1, currentVersion: 2, added: [], removed: [], reordered: [] })
  const overview = overviewFixture(1, 0); overview.discoveries.featured!.latestFeedbackAction = 'like'
  render(<FeedbackWorkflowProvider csrfToken="csrf" submitFeedback={submitFeedback} readRankingDiff={readRankingDiff} wait={async () => undefined} reload={async () => ({ kind: 'fresh', data: { adjustments: [], profile: null, overview } })}><FeedbackForm gameId={overview.discoveries.featured!.gameId} recommendationId="rec" rankingVersion={1} gameName="Game" {...options} /></FeedbackWorkflowProvider>)
  return { submitFeedback }
}

describe('FeedbackForm', () => {
  it('keeps demo reactions visible but inert', () => { const { submitFeedback } = renderForm({ readOnly: true }); fireEvent.click(screen.getByRole('button', { name: 'Нравится' })); expect(screen.getByRole('status')).toHaveTextContent('Хорошая попытка'); expect(submitFeedback).not.toHaveBeenCalled() })
  it('restores persisted reaction reasons', () => { renderForm({ initialAction: 'like', initialReasons: ['genre', 'setting'] }); expect(screen.getByText('Ранее выбрано: Нравится')).toBeInTheDocument(); expect(screen.getByText('Причины: Жанр, Сеттинг')).toBeInTheDocument() })
  it('submits multiple reasons in canonical order and optional text', async () => { const { submitFeedback } = renderForm(); fireEvent.click(screen.getByRole('button', { name: 'Не моё' })); fireEvent.click(screen.getByRole('checkbox', { name: 'Другое' })); fireEvent.click(screen.getByRole('checkbox', { name: 'Жанр' })); fireEvent.change(screen.getByLabelText('Комментарий'), { target: { value: ' note ' } }); fireEvent.click(screen.getByRole('button', { name: 'Сохранить отзыв' })); await waitFor(() => expect(submitFeedback).toHaveBeenCalledTimes(1)); expect(submitFeedback.mock.calls[0][2]).toEqual({ action: 'dislike', reasons: ['genre', 'other'], freeText: 'note', idempotencyKey: expect.any(String) }) })
  it('strips reminder reasons and text', async () => { const { submitFeedback } = renderForm(); fireEvent.click(screen.getByRole('button', { name: 'Не моё' })); fireEvent.click(screen.getByRole('checkbox', { name: 'Жанр' })); fireEvent.change(screen.getByLabelText('Комментарий'), { target: { value: 'secret' } }); fireEvent.click(screen.getByRole('button', { name: 'Напомнить' })); await waitFor(() => expect(submitFeedback).toHaveBeenCalled()); expect(submitFeedback.mock.calls[0][2]).toEqual({ action: 'remind_me', idempotencyKey: expect.any(String) }) })
  it('validates 500 Unicode code points', () => { renderForm(); fireEvent.click(screen.getByRole('button', { name: 'Не моё' })); fireEvent.change(screen.getByLabelText('Комментарий'), { target: { value: '🎮'.repeat(501) } }); fireEvent.click(screen.getByRole('button', { name: 'Сохранить отзыв' })); expect(screen.getByRole('alert')).toHaveTextContent('не длиннее 500') })
})
