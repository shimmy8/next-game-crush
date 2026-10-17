import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import SyncProgress from './SyncProgress'
import type { SyncStatus } from '../api/types'

const value = (status: string, stages: SyncStatus['stages'] = []): SyncStatus => ({ id: 'job', stage: 'steam', status, progress: 50, stages })

describe('SyncProgress', () => {
  it('prioritizes unavailable over a source error', () => {
    render(<SyncProgress value={value('succeeded', [{ kind: 'steam', stage: 'wishlist', status: 'dead', availability: 'unavailable', error: 'private' }])} />)
    expect(screen.getByRole('status')).toHaveTextContent('Источник недоступен')
    expect(screen.getByText('Недоступно')).toBeInTheDocument()
  })

  it.each([
    ['running', 'Синхронизация не завершена'],
    ['dead', 'Синхронизация завершилась с ошибкой'],
  ])('labels %s without presenting it as ready', (status, copy) => {
    render(<SyncProgress value={value(status)} />)
    expect(screen.getByRole('status')).toHaveTextContent(copy)
  })

  it('distinguishes a request failure from a job failure', () => {
    render(<SyncProgress value={value('running')} requestError="Связь прервалась" />)
    expect(screen.getByRole('status')).toHaveTextContent('Не удалось проверить статус')
  })

  it('gives a current request failure precedence while retaining unavailable stage detail', () => {
    render(<SyncProgress value={value('running', [{ kind: 'steam', stage: 'wishlist', status: 'dead', availability: 'unavailable' }])} requestError="Связь прервалась" />)
    expect(screen.getByRole('status')).toHaveTextContent('Не удалось проверить статус')
    expect(screen.getByText('Недоступно')).toBeInTheDocument()
  })

  it('labels a completed sync with a failed optional source as degraded', () => {
    render(<SyncProgress value={value('succeeded', [{ kind: 'steam', stage: 'achievements', status: 'failed', availability: 'request_failed' }])} />)
    expect(screen.getByRole('status')).toHaveTextContent('Обновлено не полностью')
  })

  it('treats ready and not_modified as current success', () => {
    render(<SyncProgress value={value('ready', [{ kind: 'steam', stage: 'catalog', status: 'ready', availability: 'not_modified' }])} />)
    expect(screen.getByRole('status')).toHaveTextContent('Синхронизация завершена')
    expect(screen.getByText('Актуально')).toBeInTheDocument()
  })
})
