import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import CalendarPage from './CalendarPage'
import type { CalendarResponse, Recommendation } from '../api/types'

const exact = {
  id: 'r-day', gameId: 'g-day', steamAppId: 480, name: 'Night Signal', storeUrl: 'https://store.steampowered.com/app/480/', profileVersion: 2, rankingVersion: 3,
  algorithmVersion: 'ranking-v1', score: 91, confidence: 0.88, deckCompatibility: 'unknown' as const,
  factors: { wishlist: 1, positiveSimilarity: 0.8, tagAffinity: 0.7 },
  weights: {}, release: { precision: 'day', exactDate: '2026-10-24', raw: '24 October 2026' },
  explanation: 'Похожая атмосфера и исследование.', evidence: [{ id: 'tags', kind: 'feature_overlap' as const, text: 'Любите atmospheric exploration' }],
	explanationSource: 'ai' as const, generatedExplanation: true, latestFeedbackAction: null, latestFeedbackReasons: null, createdAt: '2026-10-17T00:00:00Z',
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

const releaseBuckets: Array<[keyof CalendarResponse, Recommendation['release']]> = [
  ['exactDates', { precision: 'day', exactDate: '2026-10-24' }],
  ['month', { precision: 'month', raw: 'October 2026' }],
  ['quarter', { precision: 'quarter', raw: 'Q4 2026' }],
  ['year', { precision: 'year', raw: '2026' }],
  ['comingSoon', { precision: 'coming_soon', raw: 'Coming Soon' }],
  ['tba', { precision: 'unknown', raw: 'TBA' }],
]

const mixedCalendar = Object.fromEntries(releaseBuckets.map(([bucket, release]) => [bucket, [
  { source: 'Discovery', score: 80, factors: { wishlist: 0 }, platforms: { windows: true, mac: false, linux: false }, deckCompatibility: 'verified' as const },
  { source: 'Discovery low', score: 30, factors: {}, platforms: { windows: false, mac: true, linux: false }, deckCompatibility: 'playable' as const },
  { source: 'Wishlist', score: 70, factors: { wishlist: 1 }, platforms: { windows: false, mac: false, linux: true }, deckCompatibility: 'unsupported' as const },
  { source: 'Wishlist low', score: 20, factors: { wishlist: 1 }, platforms: undefined, deckCompatibility: 'unknown' as const },
].map(({ source, score, factors, platforms, deckCompatibility }) => ({
  ...exact, id: `${bucket}-${source}`, gameId: `${bucket}-${source}`, name: `${bucket} ${source}`, score, factors, platforms, deckCompatibility, release,
}))])) as CalendarResponse

afterEach(() => vi.unstubAllGlobals())

describe('CalendarPage', () => {
  it.each([
    ['month', 'Месяц', 'calendar-grid'],
    ['list', 'Список', 'release-list'],
    ['bucket', 'Месяц', 'release-bucket-month'],
  ] as const)('keeps %s reactions visible but inert in demo mode', (_region, view, testID) => {
    const fetch = vi.fn()
    vi.stubGlobal('fetch', fetch)
    render(<CalendarPage readOnly feedbackEnabled calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: view }))
    const target = screen.getByTestId(testID)
    const reaction = within(target).getAllByRole('button', { name: 'Оценить' })[0]
    expect(reaction).toBeVisible()
    fireEvent.click(reaction)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByText('Хорошая попытка — реакции пока только для владельца.')).toBeVisible()
    expect(fetch).not.toHaveBeenCalled()
  })

  it.each(['Месяц', 'Список'])('shares one dialog with posters and safe links across all releases in %s view', (view) => {
    render(<CalendarPage feedbackEnabled calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: view }))
    expect(screen.getAllByRole('button', { name: 'Оценить' })).toHaveLength(24)
    const link = screen.getByRole('link', { name: 'exactDates Discovery — открыть в Steam, новая вкладка' })
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', 'noopener noreferrer')
    expect(link.closest('.compact-game-identity')?.querySelector('.game-poster')).not.toBeNull()
    for (const button of screen.getAllByRole('button', { name: 'Оценить' })) {
      fireEvent.click(button)
      expect(screen.getAllByRole('dialog')).toHaveLength(1)
      fireEvent.click(screen.getByRole('button', { name: 'Закрыть' }))
    }
  })

  it('does not offer authenticated feedback in anonymous fixtures', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)
    expect(screen.queryByRole('button', { name: 'Оценить' })).not.toBeInTheDocument()
  })
  it('renders exact releases on their day and imprecise releases only in buckets', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)

    const day = screen.getByLabelText('24 октября 2026')
    expect(within(day).getByText('Night Signal')).toBeInTheDocument()
    expect(day).not.toHaveTextContent('Soft Orbit')
    expect(within(screen.getByTestId('release-bucket-month')).getByText('Soft Orbit')).toBeInTheDocument()
  })

  it('keeps exact releases chronological and scoped to the visible month in list mode', () => {
    render(<CalendarPage calendar={calendar} month={new Date('2026-10-01T12:00:00Z')} />)

    fireEvent.click(screen.getByRole('button', { name: 'Список' }))
    expect(screen.getByTestId('release-list')).toHaveTextContent('Night Signal')
    expect(screen.getByTestId('release-list')).not.toHaveTextContent('November Call')
    expect(screen.getByTestId('release-bucket-month')).toHaveTextContent('Soft Orbit')
    expect(screen.getByTestId('release-list').textContent?.indexOf('Amber Wake')).toBeLessThan(screen.getByTestId('release-list').textContent?.indexOf('Night Signal') ?? 0)

    fireEvent.click(screen.getByRole('button', { name: 'Месяц' }))
    expect(screen.getByTestId('calendar-grid')).toHaveTextContent('Night Signal')
    expect(screen.getByTestId('calendar-grid')).not.toHaveTextContent('November Call')
  })

  it('offers all, discovery, and wishlist sources with all selected by default', () => {
    render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)

    const source = screen.getByRole('combobox', { name: 'Источник игр' })
    expect(source).toHaveValue('all')
    expect(within(source).getAllByRole('option').map((option) => option.textContent)).toEqual(['Все', 'Новые для меня', 'В желаемом'])
    expect(screen.queryByRole('checkbox', { name: 'Только желаемое' })).not.toBeInTheDocument()
  })

  it('offers every platform and Steam Deck filter state with all selected by default', () => {
    render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)

    const platform = screen.getByRole('combobox', { name: 'Платформа' })
    expect(platform).toHaveValue('all')
    expect(within(platform).getAllByRole('option').map((option) => [option.textContent, (option as HTMLOptionElement).value])).toEqual([
      ['Все', 'all'], ['Windows', 'windows'], ['macOS', 'mac'], ['Linux', 'linux'], ['Неизвестно', 'unknown'],
    ])

    const deck = screen.getByRole('combobox', { name: 'Steam Deck' })
    expect(deck).toHaveValue('all')
    expect(within(deck).getAllByRole('option').map((option) => [option.textContent, (option as HTMLOptionElement).value])).toEqual([
      ['Все', 'all'], ['Проверено', 'verified'], ['Можно играть', 'playable'], ['Не поддерживается', 'unsupported'], ['Неизвестно', 'unknown'],
    ])
  })

  it.each(['Месяц', 'Список'])('filters all six buckets by each concrete and unknown platform state in %s view', (view) => {
    render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: view }))

    for (const [platform, expectedSuffix] of [['windows', 'Discovery'], ['mac', 'Discovery low'], ['linux', 'Wishlist'], ['unknown', 'Wishlist low']] as const) {
      fireEvent.change(screen.getByRole('combobox', { name: 'Платформа' }), { target: { value: platform } })
      for (const [bucket] of releaseBuckets) {
        for (const item of mixedCalendar[bucket]) {
          const expectedName = `${bucket} ${expectedSuffix}`
          if (item.name === expectedName) expect(screen.getByText(item.name)).toBeInTheDocument()
          else expect(screen.queryByText(item.name)).not.toBeInTheDocument()
        }
      }
    }
  })

  it.each(['Месяц', 'Список'])('filters all six buckets by every Steam Deck state in %s view', (view) => {
    render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: view }))

    for (const [deck, expectedSuffix] of [['verified', 'Discovery'], ['playable', 'Discovery low'], ['unsupported', 'Wishlist'], ['unknown', 'Wishlist low']] as const) {
      fireEvent.change(screen.getByRole('combobox', { name: 'Steam Deck' }), { target: { value: deck } })
      for (const [bucket] of releaseBuckets) {
        for (const item of mixedCalendar[bucket]) {
          const expectedName = `${bucket} ${expectedSuffix}`
          if (item.name === expectedName) expect(screen.getByText(item.name)).toBeInTheDocument()
          else expect(screen.queryByText(item.name)).not.toBeInTheDocument()
        }
      }
    }
  })

  it('treats only an absent platform object as unknown', () => {
    const allFalse = { ...exact, id: 'all-false', gameId: 'all-false', name: 'Explicitly unsupported', platforms: { windows: false, mac: false, linux: false } }
    const absent = { ...exact, id: 'absent', gameId: 'absent', name: 'Platform data absent', platforms: undefined }
    render(<CalendarPage calendar={{ exactDates: [allFalse, absent], month: [], quarter: [], year: [], comingSoon: [], tba: [] }} month={new Date('2026-10-01T12:00:00Z')} />)

    fireEvent.change(screen.getByRole('combobox', { name: 'Платформа' }), { target: { value: 'unknown' } })
    expect(screen.getByText('Platform data absent')).toBeInTheDocument()
    expect(screen.queryByText('Explicitly unsupported')).not.toBeInTheDocument()
  })

  it.each(['Месяц', 'Список'])('filters complementary sources across all six buckets in %s view', (view) => {
    render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: view }))

    for (const source of ['all', 'discoveries', 'wishlist']) {
      fireEvent.change(screen.getByRole('combobox', { name: 'Источник игр' }), { target: { value: source } })
      for (const [bucket] of releaseBuckets) {
        const target = screen.getByTestId(bucket === 'exactDates' ? (view === 'Месяц' ? 'calendar-grid' : 'release-list') : `release-bucket-${bucket}`)
        for (const item of mixedCalendar[bucket]) {
          const inWishlist = (item.factors.wishlist ?? 0) > 0
          const expected = source === 'all' || (source === 'wishlist' ? inWishlist : !inWishlist)
          if (expected) expect(within(target).getByText(item.name)).toBeInTheDocument()
          else expect(within(target).queryByText(item.name)).not.toBeInTheDocument()
        }
        if (bucket !== 'exactDates') {
          expect(target.querySelector('.release-bucket__title span')).toHaveTextContent(source === 'all' ? '4' : '2')
          expect(within(target).getAllByRole('article')).toHaveLength(source === 'all' ? 4 : 2)
        }
      }
    }
  })

  describe.each(['Месяц', 'Список'])('source, score, and precision intersection in %s view', (view) => {
    it.each(releaseBuckets)('keeps only matching %s releases and removes empty buckets', (selectedBucket, release) => {
      render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
      fireEvent.click(screen.getByRole('button', { name: view }))
      fireEvent.change(screen.getByLabelText('Минимальное совпадение'), { target: { value: '60' } })
      fireEvent.change(screen.getByLabelText('Точность даты'), { target: { value: release.precision } })

      for (const source of ['discoveries', 'wishlist']) {
        fireEvent.change(screen.getByRole('combobox', { name: 'Источник игр' }), { target: { value: source } })
        fireEvent.change(screen.getByRole('combobox', { name: 'Платформа' }), { target: { value: source === 'discoveries' ? 'windows' : 'linux' } })
        fireEvent.change(screen.getByRole('combobox', { name: 'Steam Deck' }), { target: { value: source === 'discoveries' ? 'verified' : 'unsupported' } })
        const expectedName = `${selectedBucket} ${source === 'discoveries' ? 'Discovery' : 'Wishlist'}`
        for (const [bucket] of releaseBuckets) {
          for (const item of mixedCalendar[bucket]) {
            if (item.name === expectedName) expect(screen.getByText(item.name)).toBeInTheDocument()
            else expect(screen.queryByText(item.name)).not.toBeInTheDocument()
          }
          if (bucket !== 'exactDates') {
            if (bucket === selectedBucket) {
              const target = screen.getByTestId(`release-bucket-${bucket}`)
              expect(target.querySelector('.release-bucket__title span')).toHaveTextContent('1')
              expect(within(target).getAllByRole('article')).toHaveLength(1)
            } else expect(screen.queryByTestId(`release-bucket-${bucket}`)).not.toBeInTheDocument()
          }
        }
      }

      fireEvent.change(screen.getByLabelText('Минимальное совпадение'), { target: { value: '90' } })
      for (const [bucket] of releaseBuckets) {
        for (const item of mixedCalendar[bucket]) expect(screen.queryByText(item.name)).not.toBeInTheDocument()
        if (bucket !== 'exactDates') expect(screen.queryByTestId(`release-bucket-${bucket}`)).not.toBeInTheDocument()
      }
    })
  })

  it.each(['Месяц', 'Список'])('keeps calendar items beyond dashboard collection limits in %s view', (view) => {
    const fullCalendar: CalendarResponse = {
      exactDates: [0, 1].flatMap((wishlist) => Array.from({ length: 8 }, (_, index) => ({
        ...exact, id: `${wishlist}-${index}`, gameId: `${wishlist}-${index}`,
        name: `${wishlist ? 'Wishlist' : 'Discovery'} ${index + 1}`, factors: { wishlist },
      }))),
      month: [], quarter: [], year: [], comingSoon: [], tba: [],
    }
    render(<CalendarPage calendar={fullCalendar} month={new Date('2026-10-01T12:00:00Z')} />)
    fireEvent.click(screen.getByRole('button', { name: view }))

    expect(screen.getByText('Discovery 8')).toBeInTheDocument()
    expect(screen.getByText('Wishlist 8')).toBeInTheDocument()
    fireEvent.change(screen.getByRole('combobox', { name: 'Источник игр' }), { target: { value: 'discoveries' } })
    expect(screen.getByText('Discovery 8')).toBeInTheDocument()
    expect(screen.queryByText('Wishlist 8')).not.toBeInTheDocument()
    fireEvent.change(screen.getByRole('combobox', { name: 'Источник игр' }), { target: { value: 'wishlist' } })
    expect(screen.getByText('Wishlist 8')).toBeInTheDocument()
    expect(screen.queryByText('Discovery 8')).not.toBeInTheDocument()
  })

  it('keeps every imprecise release off calendar days', () => {
    render(<CalendarPage calendar={mixedCalendar} month={new Date('2026-10-01T12:00:00Z')} />)

    const grid = screen.getByTestId('calendar-grid')
    for (const [bucket] of releaseBuckets) {
      if (bucket === 'exactDates') continue
      for (const item of mixedCalendar[bucket]) {
        expect(within(grid).queryByText(item.name)).not.toBeInTheDocument()
        expect(within(screen.getByTestId(`release-bucket-${bucket}`)).getByText(item.name)).toBeInTheDocument()
      }
    }
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

  it('shows imprecise release categories in Russian', () => {
    render(<CalendarPage calendar={{
      ...calendar,
      comingSoon: [{ ...exact, id: 'r-soon', gameId: 'g-soon', name: 'Soon Game', release: { precision: 'coming_soon', raw: 'Coming Soon' } }],
      tba: [{ ...exact, id: 'r-tba', gameId: 'g-tba', name: 'Unknown Game', release: { precision: 'unknown', raw: 'TBA' } }],
    }} month={new Date('2026-10-01T12:00:00Z')} />)

    expect(screen.getByTestId('release-bucket-comingSoon')).toHaveTextContent('Скоро')
    expect(screen.getByTestId('release-bucket-tba')).toHaveTextContent('Дата уточняется')
    expect(screen.getByLabelText('Точность даты')).toHaveTextContent('Скоро')
    expect(screen.getByLabelText('Точность даты')).toHaveTextContent('Дата уточняется')
  })
})
