import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import RecommendationCard from './RecommendationCard'
import type { Recommendation } from '../api/types'

const item = { id: 'r', gameId: 'g', name: 'Signal', profileVersion: 1, rankingVersion: 1, algorithmVersion: 'v1', score: 80, confidence: .8, factors: {}, weights: {}, release: { precision: 'unknown' }, explanation: 'Почему', evidence: [], generatedExplanation: true, createdAt: '2026-10-17T00:00:00Z' } as Recommendation

describe('RecommendationCard', () => {
  it('shows explanation provenance even when evidence is empty', () => {
    render(<RecommendationCard item={item} />)
    expect(screen.getByText(/Источник объяснения: AI/)).toBeInTheDocument()
    expect(screen.queryByText('Почему вам может понравиться')).not.toBeInTheDocument()
  })
})
