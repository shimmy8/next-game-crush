import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import DashboardPage from '../pages/DashboardPage'
import type { CurrentUser } from '../api/client'
import { overviewFixture } from '../test/overviewFixture'

const user: CurrentUser = { authenticated: true, steamId: '76561198000000000', csrfToken: 'csrf', expiresAt: '2026-10-18T00:00:00Z', displayName: 'Saved Player', avatarUrl: null }

afterEach(() => vi.unstubAllGlobals())

describe('DashboardPage states', () => {
  it('shows an actionable error without restoring the removed profile hero', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'error', message: 'Не удалось загрузить календарь' }} />)
    expect(screen.queryByRole('heading', { name: 'Во что влюбиться дальше' })).not.toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('Не удалось загрузить календарь')
    expect(screen.getByRole('button', { name: 'Повторить' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Новая находка для вас' })).not.toBeInTheDocument()
    expect(screen.getByText('Ошибка загрузки')).toBeInTheDocument()
  })

  it('does not render the empty data shell while loading', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'loading' }} />)
    expect(screen.getByRole('status')).toHaveTextContent('Загружаем профиль и календарь')
    expect(screen.queryByRole('heading', { name: 'Новая находка для вас' })).not.toBeInTheDocument()
    expect(screen.getByText('Проверяем данные')).toBeInTheDocument()
  })

  it('labels stale data and keeps it visible', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'stale', message: 'Данные от 15 октября', data: { adjustments: [], profile: null, overview: overviewFixture() } }} />)
    expect(screen.getByText('Данные от 15 октября')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1, name: 'Новая находка для вас' })).toBeInTheDocument()
    expect(screen.getByText('Пока не нашли новых игр вне вашего списка желаемого')).toBeInTheDocument()
  })

  it('keeps the profile summary only in the right rail', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{
      status: 'ready',
      data: {
        adjustments: [],
        profile: { id: 'p', version: 1, summary: 'Played activity summary.', summaryStatus: 'ready', summarySource: 'deterministic', confidence: .8, evidenceCount: 2, createdAt: '2026-10-17T00:00:00Z', publishedAt: '2026-10-17T00:00:00Z', updating: false },
        overview: overviewFixture(),
      },
    }} />)

    expect(screen.queryByRole('heading', { name: 'Ваша игровая ДНК' })).not.toBeInTheDocument()
    expect(screen.getByRole('complementary', { name: 'Профиль игры' })).toBeInTheDocument()
    expect(screen.getByText('Played activity summary.')).toBeInTheDocument()
    expect(screen.queryByText('AI-описание профиля пока недоступно. Рекомендации продолжают работать по сохранённым данным.')).not.toBeInTheDocument()
  })

  it('uses one preflight flow for both sync entry points and projects refreshed identity', async () => {
    const response = {
      canSync: false, profileVisibility: 'private', gameDetailsVisibility: 'unknown', reason: 'profile_private', displayName: 'Fresh Player', avatarUrl: null,
    }
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path === '/api/health') return new Response(JSON.stringify({ status: 'ok', components: {} }), { status: 200 })
      if (path === '/api/jobs') return new Response(JSON.stringify([]), { status: 200 })
      return new Response(JSON.stringify(response), { status: 200 })
    })
    vi.stubGlobal('fetch', fetch)
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'empty', data: { adjustments: [], profile: null, overview: overviewFixture() } }} />)

    expect(screen.getByText('Saved Player')).toBeInTheDocument()
    expect(screen.queryByText(user.steamId.slice(-6))).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Синхронизировать' }))
    await screen.findByText('Профиль Steam закрыт')
    expect(screen.getByText('Fresh Player')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Собрать профиль' }))

    await waitFor(() => expect(fetch.mock.calls.filter(([url]) => url === '/api/sync/preflight')).toHaveLength(2))
  })
})
