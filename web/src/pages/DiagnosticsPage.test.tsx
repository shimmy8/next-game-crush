import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { expect, test, vi } from 'vitest'
import DiagnosticsPage from './DiagnosticsPage'

test('shows component freshness and deletes imported data after confirmation', async () => {
  const deleteData = vi.fn().mockResolvedValue(undefined)
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({
    status: 'degraded', components: { steam: { status: 'stale', checkedAt: '2026-10-17T00:00:00Z', freshAt: '2026-10-15T00:00:00Z' } }, jobs: [{ id: 'job', kind: 'ai.profile_summary', stage: '', status: 'dead', attempts: 3, createdAt: '2026-10-17T00:00:00Z', updatedAt: '2026-10-17T00:00:00Z', error: 'synchronization stage failed' }],
  })} remove={deleteData} />)
  expect(await screen.findByText('Steam')).toBeInTheDocument()
  expect(screen.getByText('Требует обновления')).toBeInTheDocument()
  expect(screen.getByText('Краткое описание профиля')).toBeInTheDocument()
  expect(screen.queryByText('stale')).not.toBeInTheDocument()
  expect(screen.queryByText('ai.profile_summary')).not.toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: 'Удалить импортированные данные' }))
  fireEvent.click(screen.getByRole('button', { name: 'Подтвердить удаление' }))
  await waitFor(() => expect(deleteData).toHaveBeenCalledWith('csrf'))
})
