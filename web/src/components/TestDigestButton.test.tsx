import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import TestDigestButton from './TestDigestButton'

afterEach(() => vi.restoreAllMocks())

describe('TestDigestButton', () => {
  it('keeps the action visible without sending in demo mode', () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch')
    render(<TestDigestButton csrfToken="csrf-token" readOnly />)
    fireEvent.click(screen.getByRole('button', { name: 'Отправить дайджест' }))
    expect(screen.getByRole('status')).toHaveTextContent('только для просмотра')
    expect(fetchMock).not.toHaveBeenCalled()
  })
  it('sends with CSRF and reports queued status', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ deliveryId: 'delivery', status: 'pending' }), { status: 202, headers: { 'Content-Type': 'application/json' } }))
    render(<TestDigestButton csrfToken="csrf-token" />)
    fireEvent.click(screen.getByRole('button', { name: 'Отправить дайджест' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('поставлен в очередь'))
    expect(fetchMock).toHaveBeenCalledWith('/api/digests/test', expect.objectContaining({ method: 'POST', headers: { 'X-CSRF-Token': 'csrf-token' } }))
  })
})
