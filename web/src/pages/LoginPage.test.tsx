import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import LoginPage from './LoginPage'

describe('LoginPage', () => {
  it('renders the signed-out experience in Russian while preserving product names', () => {
    render(<LoginPage user={null} signingOut={false} onLogout={() => undefined} />)

    expect(screen.getByRole('heading', { name: 'Next Game Crush' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Войти через Steam' })).toHaveAttribute('href', '/auth/steam')
    expect(screen.getByText(/Steam OpenID подтверждает владение аккаунтом/)).toBeInTheDocument()
    expect(screen.queryByText(/Sign in through Steam/)).not.toBeInTheDocument()
  })

  it('renders the signed-in actions in Russian', () => {
    render(<LoginPage user={{ authenticated: true, steamId: '76561198000000000', csrfToken: 'csrf', expiresAt: '2026-10-18T00:00:00Z' }} signingOut={false} onLogout={() => undefined} />)

    expect(screen.getByText('Вы вошли как Steam ID 76561198000000000')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Выйти' })).toBeInTheDocument()
  })
})
