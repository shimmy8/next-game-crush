import { fireEvent, render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import { GamePoster } from './GamePoster'

it('uses fixed lazy decorative posters, falls back on error and resets for a new URL', () => {
  const { rerender } = render(<GamePoster name="Длинное название" coverImageUrl="https://example.com/one.jpg" size="grid" />)
  const img = screen.getByRole('img', { name: 'Обложка игры Длинное название' })
  expect(img).toHaveAttribute('width', '32')
  expect(img).toHaveAttribute('height', '48')
  expect(img).toHaveAttribute('loading', 'lazy')
  fireEvent.error(img)
  expect(screen.getByLabelText('Обложка Длинное название недоступна')).toBeInTheDocument()
  rerender(<GamePoster name="Длинное название" coverImageUrl="https://example.com/two.jpg" size="list" decorative />)
  const replacement = document.querySelector('img')!
  expect(replacement).toHaveAttribute('src', 'https://example.com/two.jpg')
  expect(replacement).toHaveAttribute('alt', '')
  expect(replacement).toHaveAttribute('width', '48')
  expect(replacement).toHaveAttribute('height', '72')
  rerender(<GamePoster name="Длинное название" coverImageUrl="https://example.com/one.jpg" size="list" decorative />)
  expect(document.querySelector('img')).toHaveAttribute('src', 'https://example.com/one.jpg')
})
