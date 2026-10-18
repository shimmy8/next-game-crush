import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import RecommendationCard, { releaseLabel } from './RecommendationCard'
import type { Recommendation } from '../api/types'

const item = { id: 'r', gameId: 'g', steamAppId: 480, name: 'Signal', coverImageUrl: 'https://shared.fastly.steamstatic.com/store_item_assets/steam/apps/480/header.jpg', storeUrl: 'https://store.steampowered.com/app/480/', deckCompatibility: 'unknown', profileVersion: 1, rankingVersion: 1, algorithmVersion: 'v1', score: 80, confidence: .8, factors: {}, weights: {}, release: { precision: 'unknown' }, explanation: 'Почему', evidence: [], explanationSource: 'ai', generatedExplanation: true, latestFeedbackAction: null, latestFeedbackReasons: null, createdAt: '2026-10-17T00:00:00Z' } as Recommendation

describe('RecommendationCard', () => {
	it('shows a persisted reaction after a ranking rebuild changes the recommendation id', () => {
		render(<RecommendationCard item={{ ...item, id: 'new-ranking-id', latestFeedbackAction: 'like' }} feedbackEnabled />)
		expect(screen.getByText('Ранее выбрано: Нравится')).toBeInTheDocument()
	})

  it('shows each supported desktop platform without inventing unsupported badges', () => {
    const { rerender } = render(<RecommendationCard item={{ ...item, platforms: { windows: true, mac: true, linux: true } }} />)
    for (const platform of ['Windows', 'macOS', 'Linux']) expect(screen.getByText(platform)).toBeInTheDocument()
    expect(screen.queryByText('Платформы неизвестны')).not.toBeInTheDocument()
    rerender(<RecommendationCard item={{ ...item, platforms: { windows: true, mac: false, linux: false } }} />)
    expect(screen.getByText('Windows')).toBeInTheDocument()
    expect(screen.queryByText('macOS')).not.toBeInTheDocument()
    expect(screen.queryByText('Linux')).not.toBeInTheDocument()
  })

  it('labels absent platforms explicitly and keeps observed all-false support distinct', () => {
    const { rerender } = render(<RecommendationCard item={item} />)
    expect(screen.getByText('Платформы неизвестны')).toBeInTheDocument()
    rerender(<RecommendationCard item={{ ...item, platforms: { windows: false, mac: false, linux: false } }} />)
    expect(screen.queryByText('Платформы неизвестны')).not.toBeInTheDocument()
  })

  it.each([
    ['verified', 'Steam Deck: проверено'],
    ['playable', 'Steam Deck: можно играть'],
    ['unsupported', 'Steam Deck: не поддерживается'],
    ['unknown', 'Steam Deck: неизвестно'],
  ] as const)('labels Deck %s with visible text', (deckCompatibility, label) => {
    render(<RecommendationCard item={{ ...item, deckCompatibility }} />)
    expect(screen.getByText(label)).toHaveClass(`compatibility-badge--${deckCompatibility}`)
  })

  it('retains accessible compatibility information before the score in compact cards', () => {
    render(<RecommendationCard item={{ ...item, platforms: { windows: false, mac: true, linux: true }, deckCompatibility: 'playable' }} compact />)
    const compatibility = screen.getByRole('group', { name: 'Совместимость игры' })
    expect(compatibility).toHaveTextContent('macOS')
    expect(compatibility).toHaveTextContent('Linux')
    expect(compatibility).toHaveTextContent('Steam Deck: можно играть')
    expect(compatibility.nextElementSibling).toHaveTextContent('совпадение')
    expect(screen.getByRole('link', { name: /Signal — открыть в Steam/ })).toBeInTheDocument()
  })

  it('keeps wishlist membership visible with zero ordering weight and leaves discovery unbadged', () => {
    const { rerender } = render(<RecommendationCard item={{ ...item, factors: { wishlist: 1 }, weights: { wishlist: 0 } }} />)
    expect(screen.getByText('♥ В желаемом')).toBeInTheDocument()
    rerender(<RecommendationCard item={item} />)
    expect(screen.queryByText(/В желаемом|не в желаемом/i)).not.toBeInTheDocument()
  })
  it('shows AI prose once, a collapsed accessible traceability disclosure, and its source', () => {
    const explanation = 'Игра близка к SOMA по атмосфере.'
    render(<RecommendationCard item={{ ...item, explanation, evidence: [
      { id: 'same', kind: 'similar_game', text: explanation },
      { id: 'feature', kind: 'feature_overlap', text: 'Совпадают темы: psychological horror' },
    ] }} />)
    expect(screen.getAllByText(explanation)).toHaveLength(1)
    const disclosure = screen.getByText('На чём основано').closest('details')
    expect(disclosure).not.toHaveAttribute('open')
    expect(disclosure).toHaveTextContent('Совпадают темы: psychological horror')
    fireEvent.click(screen.getByText('На чём основано'))
    expect(disclosure).toHaveAttribute('open')
    expect(screen.getByText('AI-сводка по вашим игровым данным')).toBeInTheDocument()
    expect(screen.queryByText('Почему вам может понравиться')).not.toBeInTheDocument()
  })

  it('keeps a safe AI explanation longer than the evidence limit', () => {
    const explanation = `Игра опирается на подтверждённые сходства. ${'Атмосфера и исследование хорошо сочетаются. '.repeat(7)}`.trim()
    expect([...explanation].length).toBeGreaterThan(240)
    render(<RecommendationCard item={{ ...item, explanation, evidence: [
      { id: 'feature', kind: 'feature_overlap', text: 'Совпадают темы: atmosphere, exploration' },
    ] }} />)
    expect(screen.getByText(explanation)).toBeInTheDocument()
    expect(screen.getByText('AI-сводка по вашим игровым данным')).toBeInTheDocument()
  })

  it('shows at most two deterministic substantive reasons once without paragraph or disclosure', () => {
    const first = 'Совпадают темы и механики: psychological horror, exploration'
    const second = 'По описанию и механикам близка к SOMA'
    render(<RecommendationCard item={{ ...item, explanationSource: 'deterministic', generatedExplanation: false, explanation: `${first}. ${second}.`, evidence: [
      { id: 'feature', kind: 'feature_overlap', text: first },
      { id: 'feature-duplicate', kind: 'positive_feedback', text: first },
      { id: 'similar', kind: 'similar_game', text: second },
      { id: 'recent', kind: 'recent_activity_anchor', text: 'Похожую игру вы запускали недавно' },
      { id: 'wishlist', kind: 'wishlist_context', text: 'Игра уже в желаемом' },
    ] }} />)
    expect(screen.getAllByText(first)).toHaveLength(1)
    expect(screen.getAllByText(second)).toHaveLength(1)
    expect(screen.queryByText(/Похожую игру вы запускали недавно|Игра уже в желаемом/)).not.toBeInTheDocument()
    expect(screen.getByRole('list')).toBeInTheDocument()
    expect(screen.queryByRole('group', { name: 'На чём основано' })).not.toBeInTheDocument()
    expect(screen.getByText('По вашим игровым данным')).toBeInTheDocument()
  })

  it('renders grounded historical completion evidence without adding completion controls', () => {
    const completion = 'Есть сходство с пройденной вами игрой SOMA.'
    render(<RecommendationCard item={{ ...item, factors: { completionAffinity: .75 }, weights: { completionAffinity: 5 }, explanationSource: 'deterministic', generatedExplanation: false, explanation: completion, evidence: [
      { id: 'completion-anchor-1', kind: 'completion_anchor', text: completion },
    ] }} />)
    expect(screen.getByText(completion)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /пройден/i })).not.toBeInTheDocument()
    expect(screen.queryByText(/completionAffinity|0\.75/)).not.toBeInTheDocument()
  })

  it('shows membership-only wishlist copy once without a why heading', () => {
    const watchlist = 'Вы уже добавили эту игру в желаемое — следим за датой выхода.'
    render(<RecommendationCard item={{ ...item, wishlisted: true, explanationSource: 'deterministic', generatedExplanation: false, explanation: watchlist, evidence: [
      { id: 'wishlist', kind: 'wishlist_context', text: watchlist },
    ] }} />)
    expect(screen.getAllByText(watchlist)).toHaveLength(1)
    expect(screen.queryByText('Почему вам может понравиться')).not.toBeInTheDocument()
    expect(screen.queryByText('На чём основано')).not.toBeInTheDocument()
  })

  it('shows exact neutral copy with no source or evidence control for none', () => {
    render(<RecommendationCard item={{ ...item, explanationSource: 'none', generatedExplanation: true, explanation: 'Недоверенный текст', evidence: [
      { id: 'feature', kind: 'feature_overlap', text: 'Недоверенный факт' },
    ] }} />)
    expect(screen.getByText('Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно.')).toBeInTheDocument()
    expect(screen.queryByText(/Недоверенный|На чём основано|Источник объяснения|игровым данным/)).not.toBeInTheDocument()
  })

  it('omits all explanation surfaces from compact cards', () => {
    render(<RecommendationCard item={{ ...item, explanation: 'AI-текст', evidence: [{ id: 'feature', kind: 'feature_overlap', text: 'Причина' }] }} compact />)
    expect(screen.queryByText(/AI-текст|Причина|На чём основано|игровым данным/)).not.toBeInTheDocument()
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
    expect(screen.getByRole('link', { name: `${item.name} — открыть в Steam, новая вкладка` })).toHaveAttribute('href', item.storeUrl)
  })

  it('does not render legacy technical explanation or evidence', () => {
    render(<RecommendationCard item={{
      ...item,
      explanation: '{"debug":true}',
      evidence: [
        { id: 'json', kind: 'similar_game', text: '{"wishlist":1}' },
        { id: 'score', kind: 'similar_game', text: 'Deterministic score: 80' },
        { id: 'date', kind: 'similar_game', text: 'coming_soon: true' },
        { id: 'mode', kind: 'similar_game', text: 'Учитываем negativeModePreference' },
        { id: 'weight', kind: 'similar_game', text: 'Учитываем negativeModePreferenceWeight' },
        { id: 'secret', kind: 'similar_game', text: 'Ключ sk-abcdefgh' },
        { id: 'hash', kind: 'similar_game', text: 'Хэш abcdef0123456789abcdef0123456789' },
        { id: 'fragment', kind: 'similar_game', text: 'Причина {"sourceGameIds":["private"]}' },
      ],
      explanationSource: 'deterministic',
      generatedExplanation: false,
    }} />)

    expect(screen.queryAllByText(/debug|wishlist|Deterministic score|coming_soon|negativeModePreference|sk-abcdefgh|abcdef0123456789|sourceGameIds/)).toHaveLength(0)
    expect(screen.getByText('Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно.')).toBeInTheDocument()
    expect(screen.queryByText('Почему вам может понравиться')).not.toBeInTheDocument()
  })
})
