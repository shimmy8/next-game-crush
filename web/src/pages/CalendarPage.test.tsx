import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import CalendarPage from './CalendarPage'
import type { CalendarResponse } from '../api/types'

const exact = {
  id: 'r-day', gameId: 'g-day', name: 'Night Signal', profileVersion: 2, rankingVersion: 3,
  algorithmVersion: 'ranking-v1', score: 91, confidence: 0.88,
  factors: { wishlist: 1, positiveSimilarity: 0.8, tagAffinity: 0.7 },
  weights: {}, release: { precision: 'day', exactDate: '2026-10-24', raw: '24 October 2026' },
  explanation: 'Похожая атмосфера и исследование.', evidence: [{ id: 'tags', text: 'Любите atmospheric exploration' }],
  generatedExplanation: true, createdAt: '2026-10-17T00:00:00Z',
}

const calendar: CalendarResponse = {
  exactDates: [
    exact,
    { ...exact, id: 'r-earlier', gameId: 'g-earlier', name: 'Amber Wake', release: { precision: 'day', exactDate: '2026-10-03' } },
    { ...exact, id: 'r-next', gameId: 'g-next', name: 'November Call', release: { precision: 'day', exactDate: '2026-11-02' } },
  ],
  month: [{ ...exact, id: 'r-month', gameId: 'g-month', name: 'Soft Orbit', score: 72, factors: { wishlist: 0 }, release: { precision: 'month', raw: 'October 2026' } }],
  quarter: [], year: [], comingSoon: [], tba: [],
}

describe('CalendarPage', () => {
  it('renders exact releases on their day and imprecise releases only in buckets', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)

    const day = screen.getByLabelText('24 октября 2026')
    expect(within(day).getByText('Night Signal')).toBeInTheDocument()
    expect(day).not.toHaveTextContent('Soft Orbit')
    expect(within(screen.getByTestId('release-bucket-month')).getByText('Soft Orbit')).toBeInTheDocument()
  })

  it('applies wishlist and score filters to the calendar and list consistently', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)

    fireEvent.click(screen.getByRole('button', { name: 'Список' }))
    expect(screen.getByTestId('release-list')).toHaveTextContent('Night Signal')
    expect(screen.getByTestId('release-list')).not.toHaveTextContent('November Call')
    expect(screen.getByTestId('release-bucket-month')).toHaveTextContent('Soft Orbit')
    expect(screen.getByTestId('release-list').textContent?.indexOf('Amber Wake')).toBeLessThan(screen.getByTestId('release-list').textContent?.indexOf('Night Signal') ?? 0)

    fireEvent.click(screen.getByRole('checkbox', { name: 'Только желаемое' }))
    expect(screen.getByTestId('release-list')).toHaveTextContent('Night Signal')
    expect(screen.queryByTestId('release-bucket-month')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Месяц' }))
    expect(screen.getByTestId('calendar-grid')).toHaveTextContent('Night Signal')
    expect(screen.queryByTestId('release-bucket-month')).not.toBeInTheDocument()
  })

  it('uses semantic table structure and a labelled view group', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)
    expect(screen.getByRole('table', { name: 'Календарь релизов' })).toBeInTheDocument()
    expect(screen.getByRole('group', { name: 'Вид календаря' })).toBeInTheDocument()
  })

  it('keeps month navigation available in list mode', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: 'Список' }))
    fireEvent.click(screen.getByRole('button', { name: 'Следующий месяц' }))
    expect(screen.getByRole('heading', { name: /ноябрь 2026/ })).toBeInTheDocument()
    expect(screen.getByTestId('release-list')).toHaveTextContent('November Call')
  })
})
