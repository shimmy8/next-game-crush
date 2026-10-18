import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import SteamIdentity from './SteamIdentity'

describe('SteamIdentity', () => {
  it('renders the Steam display name and a trusted avatar', () => {
    render(<SteamIdentity displayName="Player" avatarUrl="https://avatars.steamstatic.com/avatar.jpg" />)
    expect(screen.getByText('Player')).toBeInTheDocument()
    expect(screen.getByRole('img', { name: 'Аватар Player' })).toHaveAttribute('src', 'https://avatars.steamstatic.com/avatar.jpg')
  })

  it('keeps the name with a local fallback when the avatar is absent or fails', () => {
    const { rerender } = render(<SteamIdentity displayName="Player" avatarUrl={null} />)
    expect(screen.getByText('Player')).toBeInTheDocument()
    expect(screen.getByLabelText('Аватар Steam по умолчанию')).toBeInTheDocument()

    rerender(<SteamIdentity displayName="Player" avatarUrl="https://avatars.fastly.steamstatic.com/avatar.jpg" />)
    fireEvent.error(screen.getByRole('img', { name: 'Аватар Player' }))
    expect(screen.getByText('Player')).toBeInTheDocument()
    expect(screen.getByLabelText('Аватар Steam по умолчанию')).toBeInTheDocument()
  })

  it('never loads an unsafe avatar URL', () => {
    render(<SteamIdentity displayName={null} avatarUrl="https://avatars.cloudflare.steamstatic.com.evil.example/avatar.jpg" />)
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
    expect(screen.getByText('Игрок Steam')).toBeInTheDocument()
  })
})
