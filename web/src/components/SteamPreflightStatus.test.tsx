import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import SteamPreflightStatus from './SteamPreflightStatus'

describe('SteamPreflightStatus', () => {
  it('shows the privacy check without inventing sync progress', () => {
    render(<SteamPreflightStatus checking preflight={null} error="" onRetry={() => undefined} />)
    expect(screen.getByRole('status')).toHaveTextContent('Проверяем приватность Steam')
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  })

  it('explains required settings, hidden games, optional sources, and retries', () => {
    const retry = vi.fn()
    render(<SteamPreflightStatus checking={false} error="" onRetry={retry} preflight={{
      canSync: false, profileVisibility: 'private', gameDetailsVisibility: 'unknown', reason: 'profile_private', displayName: null, avatarUrl: null,
    }} />)
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Профиль Steam закрыт')
    expect(alert).toHaveTextContent('Откройте свой профиль Steam')
    expect(alert).toHaveTextContent('Нажмите «Редактировать профиль»')
    expect(alert).toHaveTextContent('Перейдите в «Настройки приватности»')
    expect(alert).toHaveTextContent('Мой профиль')
    expect(alert).toHaveTextContent('Доступ к игровой информации')
    expect(alert).toHaveTextContent('Сохраните изменения')
    expect(alert).toHaveTextContent('Отдельно скрытые игры останутся недоступны')
    expect(alert).toHaveTextContent('Список желаемого и достижения могут быть недоступны независимо')
    expect(screen.getAllByRole('listitem')).toHaveLength(5)
    expect(screen.getByRole('link', { name: 'Открыть настройки Steam' })).toHaveAttribute('href', 'https://steamcommunity.com/my/edit/settings')
    expect(screen.getByRole('link', { name: 'Открыть настройки Steam' })).toHaveAttribute('target', '_blank')
    expect(screen.getByRole('link', { name: 'Открыть настройки Steam' })).toHaveAttribute('rel', 'noreferrer')
    fireEvent.click(screen.getByRole('button', { name: 'Проверить снова' }))
    expect(retry).toHaveBeenCalledTimes(1)
  })

  it('uses distinct retry copy for a failed check', () => {
    render(<SteamPreflightStatus checking={false} preflight={null} error="Не удалось проверить доступ к Steam" onRetry={() => undefined} />)
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Не удалось проверить доступ к Steam')
    expect(alert).toHaveTextContent('Это не означает, что профиль закрыт')
    expect(alert).toHaveTextContent('Перейдите в «Настройки приватности»')
    expect(screen.queryByText('Профиль Steam закрыт')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Открыть настройки Steam' })).toHaveAttribute('href', 'https://steamcommunity.com/my/edit/settings')
    expect(screen.getByRole('button', { name: 'Проверить снова' })).toBeInTheDocument()
  })
})
