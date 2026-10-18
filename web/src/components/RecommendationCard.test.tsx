import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import RecommendationCard, { releaseLabel } from './RecommendationCard'
import type { Recommendation } from '../api/types'

const item = { id: 'r', gameId: 'g', steamAppId: 480, name: 'Signal', coverImageUrl: 'https://shared.fastly.steamstatic.com/store_item_assets/steam/apps/480/header.jpg', storeUrl: 'https://store.steampowered.com/app/480/', profileVersion: 1, rankingVersion: 1, algorithmVersion: 'v1', score: 80, confidence: .8, factors: {}, weights: {}, release: { precision: 'unknown' }, explanation: 'Почему', evidence: [], generatedExplanation: true, createdAt: '2026-10-17T00:00:00Z' } as Recommendation

describe('RecommendationCard', () => {
  it('shows explanation provenance even when evidence is empty', () => {
    render(<RecommendationCard item={item} />)
    expect(screen.getByText(/Источник объяснения: AI/)).toBeInTheDocument()
    expect(screen.queryByText('Почему вам может понравиться')).not.toBeInTheDocument()
  })

  it('localizes known Steam release placeholders without rewriting source dates', () => {
    expect(releaseLabel({ precision: 'coming_soon', raw: 'Coming Soon' })).toBe('Скоро')
    expect(releaseLabel({ precision: 'unknown', raw: 'TBA' })).toBe('Дата уточняется')
    expect(releaseLabel({ precision: 'month', raw: 'October 2026' })).toBe('October 2026')
  })

  it('renders a cover and a safe explicit Steam store link', () => {
    render(<RecommendationCard item={item} />)

    expect(screen.getByRole('img', { name: 'Обложка игры Signal' })).toHaveAttribute('src', item.coverImageUrl)
    expect(screen.getByRole('link', { name: 'Открыть в Steam' })).toHaveAttribute('href', item.storeUrl)
    expect(screen.getByRole('link', { name: 'Открыть в Steam' })).toHaveAttribute('target', '_blank')
    expect(screen.getByRole('link', { name: 'Открыть в Steam' })).toHaveAttribute('rel', 'noopener noreferrer')
  })

  it('shows the accessible cover fallback when the image is missing or fails to load', () => {
    const { rerender } = render(<RecommendationCard item={{ ...item, coverImageUrl: undefined }} />)
    expect(screen.getByLabelText('Обложка Signal недоступна')).toBeInTheDocument()

    rerender(<RecommendationCard item={item} />)
    fireEvent.error(screen.getByRole('img', { name: 'Обложка игры Signal' }))
    expect(screen.getByLabelText('Обложка Signal недоступна')).toBeInTheDocument()
    expect(screen.queryByRole('img', { name: 'Обложка игры Signal' })).not.toBeInTheDocument()
  })

  it('keeps the Steam link in compact cards', () => {
    render(<RecommendationCard item={item} compact />)
    expect(screen.getByRole('link', { name: 'Открыть в Steam' })).toHaveAttribute('href', item.storeUrl)
  })

  it('does not render legacy technical explanation or evidence', () => {
    render(<RecommendationCard item={{
      ...item,
      explanation: '{"debug":true}',
      evidence: [
        { id: 'json', text: '{"wishlist":1}' },
        { id: 'score', text: 'Deterministic score: 80' },
        { id: 'date', text: 'coming_soon: true' },
      ],
      generatedExplanation: false,
    }} />)

    expect(screen.queryAllByText(/debug|wishlist|Deterministic score|coming_soon/)).toHaveLength(0)
    expect(screen.getByText('Рекомендация собрана по вашим игровым предпочтениям.')).toBeInTheDocument()
    expect(screen.queryByText('Почему вам может понравиться')).not.toBeInTheDocument()
  })
})
