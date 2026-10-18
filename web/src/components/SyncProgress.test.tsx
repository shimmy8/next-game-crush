import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { SyncStatus } from '../api/types'
import SyncProgress from './SyncProgress'

const value = (status: string, stages: SyncStatus['stages'] = []): SyncStatus => ({ id: 'job', stage: 'steam', status, progress: 50, stages })

describe('SyncProgress', () => {
  it('renders active zero progress as indeterminate without a misleading percentage', () => {
    render(<SyncProgress value={{ ...value('running'), progress: 0 }} />)

    expect(screen.getByRole('progressbar', { name: 'Прогресс синхронизации' })).not.toHaveAttribute('value')
    expect(screen.queryByText('0%')).not.toBeInTheDocument()
  })

  it('keeps terminal zero progress determinate', () => {
    render(<SyncProgress value={{ ...value('failed'), progress: 0 }} />)

    expect(screen.getByRole('progressbar', { name: 'Прогресс синхронизации' })).toHaveAttribute('value', '0')
    expect(screen.getByText('0%')).toBeInTheDocument()
  })

  it('keeps active workflow status primary when an optional source is unavailable', () => {
    render(<SyncProgress value={value('running', [
      { kind: 'steam.source', stage: 'wishlist', status: 'degraded', availability: 'unavailable', error: 'private' },
    ])} />)
    expect(screen.getByRole('status')).toHaveTextContent('Синхронизация идёт')
    expect(screen.getByText('Некоторые источники недоступны — продолжаем')).toBeInTheDocument()
    expect(screen.getByText('Список желаемого')).toBeInTheDocument()
    expect(screen.getByText('Недоступно')).toBeInTheDocument()
  })

  it.each([
    ['dead', 'Синхронизация остановилась с ошибкой'],
    ['succeeded', 'Синхронизация завершена не полностью'],
  ])('uses exact terminal copy for %s', (rootStatus, copy) => {
    const stages = rootStatus === 'succeeded'
      ? [{ kind: 'steam.source', stage: 'wishlist', status: 'degraded', availability: 'request_failed' }]
      : []
    render(<SyncProgress value={value(rootStatus, stages)} />)
    expect(screen.getByRole('status')).toHaveTextContent(copy)
  })

  it('keeps backend errors sanitized and localized outside the live summary', () => {
    render(<SyncProgress value={{ ...value('dead'), error: 'synchronization stage failed' }} />)
    expect(screen.getByText('Этап синхронизации завершился с ошибкой')).toBeInTheDocument()
    expect(screen.queryByText('synchronization stage failed')).not.toBeInTheDocument()
  })

  it('shows retry as detail without replacing the active headline', () => {
    render(<SyncProgress value={value('waiting', [
      { kind: 'catalog.refresh', stage: 'catalog', status: 'retryable', error: 'temporary upstream timeout' },
    ])} />)
    expect(screen.getByRole('status')).toHaveTextContent('Синхронизация идёт')
    expect(screen.getByText('Ожидают повтора')).toBeInTheDocument()
    expect(screen.queryByText(/С ошибкой 1/)).not.toBeInTheDocument()
  })

  it('retains progress and details when the latest status request fails', () => {
    render(<SyncProgress value={value('running', [
      { kind: 'steam.source', stage: 'owned_games', status: 'ready', availability: 'available' },
    ])} requestError="Связь прервалась" />)
    expect(screen.getByRole('status')).toHaveTextContent('Не удалось обновить статус')
    expect(screen.getByRole('progressbar')).toHaveAttribute('value', '50')
    expect(screen.getByText('Библиотека Steam')).toBeInTheDocument()
  })

  it('renders grouped counts without exposing raw app IDs', () => {
    const { container } = render(<SyncProgress value={value('running', [
      { kind: 'steam.source', stage: 'achievements:620', status: 'ready', availability: 'available' },
      { kind: 'steam.source', stage: 'achievements:440', status: 'retryable' },
      { kind: 'steam.catalog', stage: 'catalog:730', status: 'ready', availability: 'not_modified' },
      { kind: 'ai.features', stage: 'ai_features', status: 'succeeded' },
      { kind: 'ai.features', stage: 'ai_features', status: 'running' },
    ])} />)
    const details = screen.getByRole('region', { name: 'Этапы синхронизации' })
    const achievements = within(details).getByText('Достижения').closest('li')
    expect(achievements).not.toBeNull()
    expect(within(achievements!).getByText(/Всего 2/)).toBeInTheDocument()
    expect(within(details).getByText('Каталог Steam')).toBeInTheDocument()
    expect(within(details).getByText('AI-признаки')).toBeInTheDocument()
    expect(container).not.toHaveTextContent(/620|440|730/)
  })

  it('has one short live summary and separately named progress and details', () => {
    render(<SyncProgress value={value('running', [
      { kind: 'steam.source', stage: 'wishlist', status: 'ready', availability: 'available' },
    ])} />)
    const liveRegions = screen.getAllByRole('status')
    expect(liveRegions).toHaveLength(1)
    expect(liveRegions[0]).toHaveAttribute('aria-live', 'polite')
    expect(liveRegions[0]).toHaveAttribute('aria-atomic', 'true')
    expect(liveRegions[0]).toHaveTextContent('Синхронизация идёт')
    expect(liveRegions[0]).not.toHaveTextContent('Список желаемого')
    expect(screen.getByRole('region', { name: 'Синхронизация идёт' })).toHaveClass('sync-progress')
    expect(screen.getByRole('progressbar', { name: 'Прогресс синхронизации' })).toHaveAttribute('value', '50')
    expect(screen.getByText('50%')).toHaveAttribute('aria-hidden', 'true')
    const details = screen.getByRole('region', { name: 'Этапы синхронизации' })
    expect(details).toHaveAttribute('tabindex', '0')
    expect(liveRegions[0]).not.toContainElement(details)
  })
})
