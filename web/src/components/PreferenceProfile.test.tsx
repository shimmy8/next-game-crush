import { render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import PreferenceProfile from './PreferenceProfile'
import { profileFixture } from '../test/overviewFixture'

it('shows the published summary and version while a newer profile is updating', () => {
  const profile = { ...profileFixture(), version: 7, summary: 'Сохранённый профиль.', updating: true }
  render(<PreferenceProfile profile={profile} />)
  expect(screen.getByRole('status')).toHaveTextContent('Профиль обновляется')
  expect(screen.getByText('Сохранённый профиль.')).toBeInTheDocument()
  expect(screen.getByText(/\u0412\u0435\u0440\u0441\u0438\u044f \u043f\u0440\u043e\u0444\u0438\u043b\u044f 7/)).toBeInTheDocument()
})

it('describes played activity evidence without wishlist or reaction language', () => {
  render(<PreferenceProfile profile={profileFixture()} />)
  const details = screen.getByText(/\u0441\u044b\u0433\u0440\u0430\u043d\u043d\u044b\u0445 \u0438\u0433\u0440/)
  expect(details).toHaveTextContent('недавней активности')
  expect(details).toHaveTextContent('достижений')
  expect(details).toHaveTextContent('завершений')
  expect(details).not.toHaveTextContent(/желаем|реакц/i)
})

it('does not invent a fallback summary when no profile has published', () => {
  render(<PreferenceProfile profile={null} />)
  expect(screen.getByText(/\u0421\u0438\u043d\u0445\u0440\u043e\u043d\u0438\u0437\u0438\u0440\u0443\u0439\u0442\u0435 Steam/)).toBeInTheDocument()
  expect(screen.queryByText(/\u041f\u0440\u043e\u0444\u0438\u043b\u044c \u0441\u043e\u0431\u0440\u0430\u043d/)).not.toBeInTheDocument()
})
