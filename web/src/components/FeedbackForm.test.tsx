import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import FeedbackForm from './FeedbackForm'
import type { RankingDiff } from '../api/types'

describe('FeedbackForm', () => {
  it('submits a predefined reason and optional text, then reveals the server diff', async () => {
    const submit = vi.fn().mockResolvedValue({ eventId: 'event-1', rebuildJobId: 'job-1', previousRankingVersion: 4 })
		const diff: RankingDiff = { rebuildStatus: 'succeeded', previousVersion: 4, currentVersion: 5, added: ['new'], removed: ['old'], reordered: ['moved'], classification: { taxonomy: 'setting', polarity: 'positive', confidence: .4, actionable: false } }
    const readDiff = vi.fn().mockResolvedValue(diff)
    const completed = vi.fn()
    let releaseWait = () => {}
    const wait = () => new Promise<void>((resolve) => { releaseWait = resolve })
    render(<FeedbackForm recommendationId="rec-1" rankingVersion={4} csrfToken="csrf" submit={submit} readDiff={readDiff} wait={wait} onRankingUpdated={completed} />)

    fireEvent.click(screen.getByRole('button', { name: 'Нравится' }))
    fireEvent.change(screen.getByLabelText('Причина'), { target: { value: 'setting' } })
    fireEvent.change(screen.getByLabelText('Комментарий'), { target: { value: 'Хочу больше таких миров' } })
    fireEvent.click(screen.getByRole('button', { name: 'Сохранить отзыв' }))

    expect(await screen.findByText('Отзыв сохранён')).toBeInTheDocument()
    expect(screen.getByText('Пересобираем рекомендации…')).toBeInTheDocument()
    releaseWait()
    await waitFor(() => expect(completed).toHaveBeenCalledWith(diff))
    expect(await screen.findByText(/Добавлено: 1/)).toBeInTheDocument()
    expect(screen.getByText(/Убрано: 1/)).toBeInTheDocument()
    expect(screen.getByText(/Переставлено: 1/)).toBeInTheDocument()
		expect(screen.getByText(/40% · сохранён без влияния/)).toBeInTheDocument()
		expect(readDiff).toHaveBeenCalledWith(4, 'job-1', expect.anything())
		expect(submit).toHaveBeenCalledWith('rec-1', 'csrf', expect.objectContaining({ action: 'like', reason: 'setting', freeText: 'Хочу больше таких миров' }), expect.anything())
  })

  it('exposes a 500 character limit and an accessible validation error', async () => {
    render(<FeedbackForm recommendationId="rec-1" rankingVersion={4} csrfToken="csrf" submit={vi.fn()} readDiff={vi.fn()} wait={() => Promise.resolve()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Не моё' }))
    const text = screen.getByLabelText('Комментарий')
    expect(text).toHaveAttribute('maxLength', '500')
    fireEvent.change(text, { target: { value: 'x'.repeat(501) } })
    fireEvent.click(screen.getByRole('button', { name: 'Сохранить отзыв' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('не длиннее 500')
  })

	it('keeps the saved state when diff polling fails and offers a retry', async () => {
		const submit = vi.fn().mockResolvedValue({ eventId: 'event-1', rebuildJobId: 'job-1', previousRankingVersion: 4 })
		const readDiff = vi.fn().mockRejectedValue(new Error('offline'))
		render(<FeedbackForm recommendationId="rec-1" rankingVersion={4} csrfToken="csrf" submit={submit} readDiff={readDiff} wait={() => Promise.resolve()} />)
		fireEvent.click(screen.getByRole('button', { name: 'Напомнить' }))
		expect(await screen.findByText('Напоминание сохранено')).toBeInTheDocument()
		expect(await screen.findByRole('alert')).toHaveTextContent('статус пересборки пока недоступен')
		expect(screen.getByRole('button', { name: 'Повторить проверку' })).toBeInTheDocument()
	})
})
