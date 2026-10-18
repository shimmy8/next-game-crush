import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import ReleaseCalendar from './ReleaseCalendar'
import { recommendation, reviewFixture } from '../test/overviewFixture'

const item = { ...recommendation('Night Signal'), release: { precision: 'day', exactDate: '2026-10-18' } }
const month = new Date('2026-10-01T00:00:00Z')

describe('ReleaseCalendar reviews', () => {
  it('exposes reviews in an accessible compact detail without changing the personal score', () => {
    render(<ReleaseCalendar month={month} items={[{ ...item, reviews: reviewFixture }]} />)
    const disclosure = screen.getByText('Отзывы Steam').closest('details')
    expect(disclosure).not.toHaveAttribute('open')
    fireEvent.click(screen.getByText('Отзывы Steam'))
    expect(disclosure).toHaveAttribute('open')
    expect(screen.getByLabelText('Отзывы игроков в Steam: 91% положительных · 1 284 отзыва')).toHaveTextContent('91% положительных · 1 284 отзыва')
    expect(screen.getByText(`${item.score}%`)).toBeInTheDocument()
  })

  it('shows explicit unrated detail but omits the detail for missing reviews', () => {
    const { rerender } = render(<ReleaseCalendar month={month} items={[{ ...item, reviews: { ...reviewFixture, band: 'unrated', label: 'No user reviews', positivePercent: null, total: 0 } }]} />)
    expect(screen.getByText('Пока нет отзывов')).toBeInTheDocument()
    rerender(<ReleaseCalendar month={month} items={[item]} />)
    expect(screen.queryByText('Отзывы Steam')).not.toBeInTheDocument()
  })
})
