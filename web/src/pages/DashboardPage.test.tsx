import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import DashboardPage from './DashboardPage'
import type { CurrentUser, RecommendationOverview } from '../api/types'
import { overviewFixture, profileFixture } from '../test/overviewFixture'
import { applyFeedbackTombstones } from '../components/feedbackVisibility'

const user: CurrentUser = { authenticated: true, steamId: '76561198000000000', csrfToken: 'csrf', expiresAt: '2026-10-18T00:00:00Z' }

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    if (String(input) === '/api/sync/current') return new Response(null, { status: 204 })
    return new Response(JSON.stringify(String(input) === '/api/jobs' ? [] : { status: 'ok', components: {} }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

function show(overview: RecommendationOverview) {
  render(<DashboardPage user={user} signingOut={false} onLogout={() => undefined} initialState={{ status: 'ready', data: { profile: profileFixture(), overview } }} />)
}

it('shows demo banner and intercepts visible mutation controls', () => {
  const demoUser: CurrentUser = { authenticated: true, accessMode: 'demo', csrfToken: 'csrf', expiresAt: '2026-10-18T00:00:00Z' }
  const fetchMock = vi.mocked(fetch)
  render(<DashboardPage user={demoUser} signingOut={false} onLogout={() => undefined} initialState={{ status: 'ready', data: { profile: profileFixture(), overview: overviewFixture(1, 0) } }} />)
  expect(screen.getByRole('status', { name: 'Демо-режим' })).toHaveTextContent('реальные данные владельца')
  fireEvent.click(screen.getByRole('button', { name: 'Синхронизировать' }))
  expect(screen.getByText(/только для просмотра/i)).toBeInTheDocument()
  expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'POST' || init?.method === 'DELETE')).toBe(false)
})

describe('DashboardPage collections', () => {
	it('removes an accepted dislike from dashboard and calendar while its rebuild is pending', async () => {
		const overview = overviewFixture(2, 0)
		const targetGameId = overview.discoveries.featured!.gameId
		const freshOverview = applyFeedbackTombstones(overview, new Set([targetGameId]))
		let finishDiff!: (response: Response) => void
		vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			if (init?.method === 'POST' && String(input).includes('/feedback')) return new Response(JSON.stringify({ eventId: 'event', rebuildJobId: 'job', previousRankingVersion: 1 }))
			if (String(input).includes('/diff?')) return new Promise<Response>((resolve) => { finishDiff = resolve })
			if (String(input) === '/api/profile') return new Response(JSON.stringify(profileFixture()))
			if (String(input) === '/api/recommendations/overview') return new Response(JSON.stringify(freshOverview))
			return new Response(null, { status: 204 })
		}))
		show(overview)
		const hero = screen.getByRole('region', { name: 'Новая находка для вас' })
		fireEvent.click(within(hero).getByRole('button', { name: 'Не моё' }))
		fireEvent.click(within(hero).getByRole('button', { name: 'Сохранить отзыв' }))
		await waitFor(() => expect(screen.queryByRole('heading', { name: 'Discovery 1' })).not.toBeInTheDocument())
		expect(screen.getByRole('heading', { name: 'Discovery 2' })).toBeInTheDocument()
		await waitFor(() => expect(finishDiff).toBeTypeOf('function'))
		finishDiff(new Response(JSON.stringify({ rebuildStatus: 'succeeded', previousVersion: 1, currentVersion: 2, added: [], removed: ['r'], reordered: [] })))
		await waitFor(() => expect(screen.getByText(/Рекомендации обновлены · версия 2/)).toBeInTheDocument())
	})

	it('serializes reactions for the same game across dashboard and calendar surfaces', async () => {
		const overview = overviewFixture(2, 0)
		let acceptDislike!: (response: Response) => void
		const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			if (init?.method === 'POST' && String(input).includes('/feedback')) return new Promise<Response>((resolve) => { acceptDislike = resolve })
			return new Promise<Response>(() => undefined)
		})
		vi.stubGlobal('fetch', fetch)
		show(overview)
		const hero = screen.getByRole('region', { name: 'Новая находка для вас' })
		fireEvent.click(within(hero).getByRole('button', { name: 'Не моё' }))
		fireEvent.click(within(hero).getByRole('button', { name: 'Сохранить отзыв' }))
		await waitFor(() => expect(fetch.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1))
		const calendarCard = screen.getAllByRole('article').find((article) => within(article).queryByRole('button', { name: 'Оценить' }) && article.textContent?.includes('Discovery 1'))
		const calendarLauncher = within(calendarCard!).getByRole('button', { name: 'Оценить' })
		expect(calendarLauncher).toBeDisabled()
		fireEvent.click(calendarLauncher)
		expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
		expect(fetch.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1)
		acceptDislike(new Response(JSON.stringify({ eventId: 'event', rebuildJobId: 'job', previousRankingVersion: 1 })))
	})
  it('renders the server discovery hero, six discoveries and six secondary wishlist cards with full totals', () => {
    const overview = overviewFixture(8, 8)
    overview.discoveries.items.reverse()
    show(overview)
    const hero = screen.getByRole('region', { name: 'Новая находка для вас' })
    const more = screen.getByRole('region', { name: 'Ещё новые игры' })
    const wishlist = screen.getByRole('region', { name: 'Уже в вашем желаемом' })
    expect(within(hero).getAllByRole('article')).toHaveLength(1)
    expect(within(hero).getByRole('heading', { name: 'Discovery 1' })).toBeInTheDocument()
    expect(within(more).getAllByRole('article')).toHaveLength(6)
    expect(within(more).getAllByRole('heading', { level: 3 }).map((node) => node.textContent)).toEqual(overview.discoveries.items.map((item) => item.name))
    expect(within(wishlist).getAllByRole('article')).toHaveLength(6)
    expect(within(hero).queryByText('♥ В желаемом')).not.toBeInTheDocument()
    expect(within(more).queryByText('♥ В желаемом')).not.toBeInTheDocument()
    expect(within(wishlist).getAllByText('♥ В желаемом')).toHaveLength(6)
    expect(within(more).getByText('Всего новых игр: 8')).toBeInTheDocument()
    expect(within(wishlist).getByText('Всего в желаемом: 8')).toBeInTheDocument()
    expect(screen.queryByText(/не в желаемом/i)).not.toBeInTheDocument()
    expect(screen.getByText('Календарь релизов')).toBeInTheDocument()
    // Bucket listings retain candidates outside both dashboard caps.
    expect(screen.getByText('Discovery 8')).toBeInTheDocument()
    expect(screen.getByText('Wishlist 8')).toBeInTheDocument()
    for (const section of [hero, more, wishlist]) {
      const articles = within(section).getAllByRole('article')
      for (const article of articles) expect(within(article).getByRole('button', { name: 'Напомнить' })).toBeInTheDocument()
    }
  })

  it.each([[3, 0], [0, 3], [0, 0]])('keeps discovery-only, wishlist-only and empty states honest (%s/%s)', (discoveries, wishlist) => {
    show(overviewFixture(discoveries, wishlist))
    const hero = screen.getByRole('region', { name: 'Новая находка для вас' })
    const watchlist = screen.getByRole('region', { name: 'Уже в вашем желаемом' })
    if (!discoveries) {
      expect(within(hero).getByText('Пока не нашли новых игр вне вашего списка желаемого')).toBeInTheDocument()
      expect(within(hero).queryByRole('article')).not.toBeInTheDocument()
      expect(within(hero).getByRole('button', { name: 'Собрать профиль' })).toBeInTheDocument()
    }
    expect(within(watchlist).queryAllByRole('article')).toHaveLength(wishlist)
    expect(screen.getByText('Календарь релизов')).toBeInTheDocument()
    if (!wishlist) expect(within(watchlist).getByText('В желаемом нет подходящих будущих релизов.')).toBeInTheDocument()
  })

  it.each(['stale', 'unavailable', 'unknown'] as const)('does not present %s wishlist as a successful empty response', (status) => {
    const overview = overviewFixture(0, status === 'stale' ? 3 : 0)
    overview.wishlist = { ...overview.wishlist, status, availability: 'unavailable', degradedReason: 'private_or_unavailable', lastSuccessfulFetchAt: status === 'stale' ? '2026-10-17T10:00:00Z' : null }
    overview.catalogStatus.status = 'stale'
    show(overview)
    const watchlist = screen.getByRole('region', { name: 'Уже в вашем желаемом' })
    expect(within(watchlist).queryByText('В желаемом нет подходящих будущих релизов.')).not.toBeInTheDocument()
    expect(watchlist).toHaveTextContent(status === 'stale' ? 'Показываем сохранённый список желаемого' : status === 'unavailable' ? 'Список желаемого недоступен' : 'Список желаемого ещё не проверен')
    if (status === 'stale') {
      expect(watchlist).toHaveTextContent('17 октября 2026')
      expect(within(watchlist).getAllByRole('article')).toHaveLength(3)
    }
    expect(screen.getByRole('complementary', { name: 'Охват каталога' })).toHaveTextContent('Каталог давно не обновлялся')
    expect(screen.getByText('Данные актуальны')).toBeInTheDocument()
  })
})
