import { fireEvent, render, screen, within } from '@testing-library/react'
import { expect, it } from 'vitest'
import FeedbackLauncher from './FeedbackLauncher'

it('opens one named modal, traps focus, marks the background inert and returns focus', () => {
  render(<FeedbackLauncher recommendationId="rec" rankingVersion={1} gameName="Long Кириллица" />)
  const opener = screen.getByRole('button', { name: 'Оценить' })
  opener.focus(); fireEvent.click(opener)
  const dialog = screen.getByRole('dialog', { name: 'Оценить Long Кириллица' })
  const close = within(dialog).getByRole('button', { name: 'Закрыть' })
  expect(close).toHaveFocus()
  expect(opener.closest('[inert]')).not.toBeNull()
  fireEvent.keyDown(dialog, { key: 'Tab', shiftKey: true })
  expect(within(dialog).getByRole('button', { name: 'Напомнить' })).toHaveFocus()
  fireEvent.keyDown(dialog, { key: 'Tab' })
  expect(close).toHaveFocus()
  fireEvent.keyDown(dialog, { key: 'Escape' })
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  expect(opener).toHaveFocus()
  expect(opener.closest('[inert]')).toBeNull()
})

it('keeps the launcher visible but does not open a dialog in demo mode', () => {
  render(<FeedbackLauncher recommendationId="rec" rankingVersion={1} gameName="Demo" readOnly />)
  fireEvent.click(screen.getByRole('button', { name: 'Оценить' }))
  expect(screen.getByRole('status')).toHaveTextContent('Хорошая попытка')
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})
