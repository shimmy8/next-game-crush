import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import DashboardPage from '../pages/DashboardPage'
import type { CurrentUser } from '../api/client'

const user: CurrentUser = { authenticated: true, steamId: '76561198000000000', csrfToken: 'csrf', expiresAt: '2026-10-18T00:00:00Z', displayName: 'Saved Player', avatarUrl: null }

afterEach(() => vi.unstubAllGlobals())

describe('DashboardPage states', () => {
  it('shows an actionable error without hiding the journal shell', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'error', message: 'Не удалось загрузить календарь' }} />)
    expect(screen.getByRole('heading', { name: 'Во что влюбиться дальше' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('Не удалось загрузить календарь')
    expect(screen.getByRole('button', { name: 'Повторить' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Ваш следующий фаворит' })).not.toBeInTheDocument()
    expect(screen.getByText('Ошибка загрузки')).toBeInTheDocument()
  })

  it('does not render the empty data shell while loading', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'loading' }} />)
    expect(screen.getByRole('status')).toHaveTextContent('Загружаем профиль и календарь')
    expect(screen.queryByRole('heading', { name: 'Ваш следующий фаворит' })).not.toBeInTheDocument()
    expect(screen.getByText('Проверяем данные')).toBeInTheDocument()
  })

  it('labels stale data and keeps it visible', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'stale', message: 'Данные от 15 октября', data: { profile: null, calendar: { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] } } }} />)
    expect(screen.getByText('Данные от 15 октября')).toBeInTheDocument()
    expect(screen.getByText('Пока рекомендаций нет')).toBeInTheDocument()
  })

  it('shows neutral localized copy when the AI profile summary is unavailable', () => {
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{
      status: 'ready',
      data: {
        profile: { id: 'p', version: 1, summary: '', summaryStatus: 'unavailable', confidence: .8, evidenceCount: 2, createdAt: '2026-10-17T00:00:00Z' },
        calendar: { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] },
      },
    }} />)

    expect(screen.getByText('AI-описание профиля пока недоступно. Рекомендации продолжают работать по сохранённым данным.')).toBeInTheDocument()
    expect(screen.queryByText(/AI profile summary is unavailable/i)).not.toBeInTheDocument()
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
    render(<DashboardPage user={user} onLogout={() => undefined} signingOut={false} initialState={{ status: 'empty', data: { profile: null, calendar: { exactDates: [], month: [], quarter: [], year: [], comingSoon: [], tba: [] } } }} />)

    expect(screen.getByText('Saved Player')).toBeInTheDocument()
    expect(screen.queryByText(user.steamId.slice(-6))).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Синхронизировать' }))
    await screen.findByText('Профиль Steam закрыт')
    expect(screen.getByText('Fresh Player')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Собрать профиль' }))

    await waitFor(() => expect(fetch.mock.calls.filter(([url]) => url === '/api/sync/preflight')).toHaveLength(2))
  })
})
