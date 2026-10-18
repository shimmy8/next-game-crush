import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import PreferenceAdjustments from './PreferenceAdjustments'
import type { PreferenceAdjustment } from '../api/types'

const rule: PreferenceAdjustment = { id: 'opaque-rule', dimension: 'mode', target: 'co-op', label: 'Кооперативные игры', effect: 'понижаем в рекомендациях', active: true, confidence: .92, createdAt: '2026-10-18T00:00:00Z' }
const accepted = { adjustmentId: 'opaque-rule', rebuildJobId: 'job', previousRankingVersion: 4, alreadyInactive: false }
const diff = { rebuildStatus: 'succeeded', previousVersion: 4, currentVersion: 5, added: [], removed: [], reordered: [] }

it('shows active human labels only and handles empty normalized data', () => {
  const { rerender } = render(<PreferenceAdjustments adjustments={[rule, { ...rule, id: 'inactive-id', label: 'Одиночные игры', active: false }]} csrfToken="csrf" />)
  expect(screen.getByText('Учитываем ваши корректировки')).toBeInTheDocument()
  expect(screen.getByText('Кооперативные игры')).toBeInTheDocument()
  expect(screen.queryByText('Одиночные игры')).not.toBeInTheDocument()
  expect(screen.queryByText('opaque-rule')).not.toBeInTheDocument()
  rerender(<PreferenceAdjustments adjustments={[]} csrfToken="csrf" />)
  expect(screen.queryByText('Учитываем ваши корректировки')).not.toBeInTheDocument()
})

it('keeps correction controls visible but read-only in demo mode', () => {
  const disable = vi.fn()
  render(<PreferenceAdjustments adjustments={[rule]} csrfToken="csrf" disable={disable} readOnly />)
  fireEvent.click(screen.getByRole('button', { name: 'Не учитывать: Кооперативные игры' }))
  expect(screen.getByRole('status')).toHaveTextContent(/демо-режим/i)
  expect(disable).not.toHaveBeenCalled()
})

it('disables once, retries polling with its accepted metadata and refreshes once', async () => {
  const disable = vi.fn().mockResolvedValue(accepted)
  const readDiff = vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValue(diff)
  const updated = vi.fn()
  render(<PreferenceAdjustments adjustments={[rule]} csrfToken="csrf" disable={disable} readDiff={readDiff} wait={() => Promise.resolve()} onRankingUpdated={updated} />)
  const button = screen.getByRole('button', { name: 'Не учитывать: Кооперативные игры' })
  fireEvent.click(button); fireEvent.click(button)
  expect(await screen.findByRole('alert')).toHaveTextContent('статус пересборки')
  fireEvent.click(screen.getByRole('button', { name: 'Повторить проверку: Кооперативные игры' }))
  await waitFor(() => expect(updated).toHaveBeenCalledTimes(1))
  expect(disable).toHaveBeenCalledTimes(1)
  expect(disable).toHaveBeenCalledWith('opaque-rule', 'csrf', expect.any(AbortSignal))
  expect(readDiff.mock.calls.every(([version, job]) => version === 4 && job === 'job')).toBe(true)
  expect(button).toBeDisabled()
  expect(screen.getByText('Кооперативные игры')).toBeInTheDocument()
})

it('keeps an active row after a save error and never reports a dead rebuild as success', async () => {
  const disable = vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValue(accepted)
  const updated = vi.fn()
  render(<PreferenceAdjustments adjustments={[rule]} csrfToken="csrf" disable={disable} readDiff={vi.fn().mockResolvedValue({ ...diff, rebuildStatus: 'dead' })} wait={() => Promise.resolve()} onRankingUpdated={updated} />)
  fireEvent.click(screen.getByRole('button', { name: 'Не учитывать: Кооперативные игры' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('Не удалось отключить')
  fireEvent.click(screen.getByRole('button', { name: 'Не учитывать: Кооперативные игры' }))
  await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('завершилась с ошибкой'))
  expect(updated).not.toHaveBeenCalled()
})
