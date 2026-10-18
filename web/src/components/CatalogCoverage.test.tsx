import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import CatalogCoverage from './CatalogCoverage'
import type { CatalogStatus } from '../api/types'

describe('CatalogCoverage', () => {
  it.each([
    ['ready', 'Ограниченный обход каталога завершён'],
    ['backfilling', 'Каталог обновляется'],
    ['stale', 'Каталог давно не обновлялся'],
    ['unavailable', 'Обновление каталога недоступно'],
    ['disabled', 'Используется проверенный локальный каталог'],
  ] as const)('presents %s with aggregate coverage only', (status, copy) => {
    render(<CatalogCoverage status={{ status, activeDiscoveredGames: 42, lastSuccessfulScanAt: null, backfillCompletedAt: null }} />)
    const region = screen.getByRole('complementary', { name: 'Охват каталога' })
    expect(region).toHaveTextContent(copy)
    expect(region).toHaveTextContent('42')
    expect(region).toHaveTextContent('Последнее обновление: неизвестно')
    expect(region).not.toHaveTextContent(/cursor|AppID|весь Steam/i)
    if (status === 'backfilling') expect(region).toHaveTextContent('Каталог может быть неполным')
    if (status === 'stale' || status === 'unavailable') expect(region).toHaveTextContent('Последние сохранённые рекомендации остаются видны')
    if (status === 'disabled') expect(region).not.toHaveTextContent('обход каталога завершён')
  })

  it('localizes a successful scan time and does not invent missing completion dates', () => {
    const status: CatalogStatus = { status: 'ready', activeDiscoveredGames: 42, lastSuccessfulScanAt: '2026-10-17T10:00:00Z', backfillCompletedAt: null }
    render(<CatalogCoverage status={status} />)
    expect(screen.getByText(/Последнее обновление:/)).toHaveTextContent('17 октября 2026')
    expect(screen.getByText(/Последнее обновление:/).querySelector('time')).toHaveAttribute('datetime', status.lastSuccessfulScanAt)
    expect(screen.queryByText(/Invalid Date/)).not.toBeInTheDocument()
  })
})
