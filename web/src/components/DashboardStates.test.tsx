import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import DashboardPage from '../pages/DashboardPage'
import type { CurrentUser } from '../api/client'

const user: CurrentUser = { authenticated: true, steamId: '76561198000000000', csrfToken: 'csrf', expiresAt: '2026-10-18T00:00:00Z' }

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
})
