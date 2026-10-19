import { fireEvent, render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import { ExplicitRecommendationSettings } from './ExplicitRecommendationSettings'
import type { PreferenceAdjustment } from '../api/types'

const adjustment: PreferenceAdjustment = { id: 'a', dimension: 'mode', target: 'co-op', label: 'Кооперативные игры', effect: 'понижаем в рекомендациях', active: true, confidence: .9, createdAt: '2026-10-19T00:00:00Z' }

it('renders active corrections in a separate explicitly named section', () => {
  render(<ExplicitRecommendationSettings adjustments={[adjustment]} csrfToken="csrf" />)
  const section = screen.getByRole('complementary', { name: 'Явные настройки рекомендаций' })
  expect(section).toHaveTextContent('Кооперативные игры')
  expect(section).toHaveTextContent('не изменяют профиль игры')
})

it('keeps disable controls available independently of profile refresh state', () => {
  render(<ExplicitRecommendationSettings adjustments={[adjustment]} csrfToken="csrf" readOnly />)
  fireEvent.click(screen.getByRole('button', { name: 'Не учитывать: Кооперативные игры' }))
  expect(screen.getByRole('status')).toHaveTextContent(/демо-режим/i)
})

it('renders nothing without active corrections', () => {
  const { container } = render(<ExplicitRecommendationSettings adjustments={[]} csrfToken="csrf" />)
  expect(container).toBeEmptyDOMElement()
})
