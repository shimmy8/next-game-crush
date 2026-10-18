import { fireEvent, render, screen } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import FeedbackDialog from './FeedbackDialog'
import FeedbackWorkflowProvider from './FeedbackWorkflowProvider'

it('traps focus, closes on Escape, and restores focus', () => {
  const opener = document.createElement('button'); document.body.append(opener); opener.focus(); const onClose = vi.fn()
  const wrapper = (open: boolean) => <FeedbackWorkflowProvider csrfToken="csrf" reload={async () => ({ kind: 'failed' as const, reason: 'request_failed' as const })}><FeedbackDialog open={open} gameId="game" gameName="Game" recommendationId="rec" rankingVersion={1} onClose={onClose} returnFocusTo={opener} /></FeedbackWorkflowProvider>
  const { rerender } = render(wrapper(true)); expect(screen.getByRole('dialog')).toBeInTheDocument(); fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' }); expect(onClose).toHaveBeenCalled(); rerender(wrapper(false)); expect(opener).toHaveFocus(); opener.remove()
})
