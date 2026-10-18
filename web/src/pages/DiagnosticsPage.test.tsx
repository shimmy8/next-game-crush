import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { expect, test, vi } from 'vitest'
import DiagnosticsPage from './DiagnosticsPage'

test('keeps deletion visible without mutating in demo mode', async () => {
  const remove = vi.fn()
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({ status: 'ready', components: {}, jobs: [] })} loadAdjustments={vi.fn().mockResolvedValue([])} remove={remove} readOnly />)
  fireEvent.click(screen.getByRole('button', { name: 'Удалить импортированные данные' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('только для просмотра')
  expect(remove).not.toHaveBeenCalled()
})

test('requests inactive preference history separately and displays human status without IDs', async () => {
  const loadAdjustments = vi.fn().mockResolvedValue([{ id: 'hidden-rule-id', dimension: 'mode', target: 'co-op', label: 'Кооперативные игры', effect: 'понижаем в рекомендациях', active: false, confidence: .9, createdAt: '2026-10-18T00:00:00Z' }])
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({ status: 'ready', components: {}, jobs: [] })} loadAdjustments={loadAdjustments} />)
  expect(await screen.findByText('Не учитывается')).toBeInTheDocument()
  expect(screen.getByText('Кооперативные игры')).toBeInTheDocument()
  expect(screen.queryByText('hidden-rule-id')).not.toBeInTheDocument()
  expect(loadAdjustments).toHaveBeenCalledWith(true, expect.any(AbortSignal))
})

test('adjustment history failure stays local to diagnostics', async () => {
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({ status: 'ready', components: { steam: { status: 'ready', checkedAt: '' } }, jobs: [] })} loadAdjustments={vi.fn().mockRejectedValue(new Error('offline'))} />)
  expect(await screen.findByText('История корректировок временно недоступна')).toBeInTheDocument()
  expect(screen.getByText('Steam')).toBeInTheDocument()
})

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

test.each([
  ['updating', '2026-10-18T00:00:00Z', 'Каталог обновляется'],
  ['stale', null, 'Каталог может быть неполным'],
  ['degraded', null, 'Каталог может быть неполным'],
] as const)('shows safe catalog coverage copy for %s', async (status, backfillCompletedAt, expected) => {
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({
    status: 'ready', components: {}, jobs: [], catalogDiscovery: {
      status, activeDiscoveredGames: 12, lastSuccessfulScanAt: null, backfillCompletedAt, backfillCursor: 77,
      latestRun: { discovered: 4, evaluated: 3, admitted: 1, skipped: 1, failed: 1 }, activeBySource: { steam_discovery: 12 }, pendingQualifications: 2,
    },
  })} />)
  expect(await screen.findByText(expected)).toBeInTheDocument()
  expect(screen.getByText('12')).toBeInTheDocument()
  expect(screen.queryByText(/полный каталог steam/i)).not.toBeInTheDocument()
})

test('shows allowlisted aggregate explanation diagnostics in Russian', async () => {
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({
    status: 'ready', components: {}, jobs: [{
      id: 'rebuild', kind: 'recommendations.rebuild', stage: '', status: 'succeeded', attempts: 1,
      createdAt: '2026-10-18T00:00:00Z', updatedAt: '2026-10-18T00:00:00Z',
      explanations: {
        aiGenerated: 12, deterministic: 15, none: 11,
        failureReasons: {
          disabled: 1, rate_limited: 2, timeout: 3, provider_unavailable: 4, refusal: 5,
          invalid_output: 6, invalid_citation: 7, unsafe_text: 8, unknown: 9,
          private_provider_message: 100,
        },
      },
    }],
  })} loadAdjustments={vi.fn().mockResolvedValue([])} />)
  expect(await screen.findByText('Объяснения рекомендаций')).toBeInTheDocument()
  expect(screen.getByText('AI-сводки')).toBeInTheDocument()
  expect(screen.getByText('12')).toBeInTheDocument()
  expect(screen.getByText('По правилам')).toBeInTheDocument()
  expect(screen.getByText('15')).toBeInTheDocument()
  expect(screen.getByText('Нейтральные')).toBeInTheDocument()
  expect(screen.getByText('OpenAI отключён')).toBeInTheDocument()
  expect(screen.getByText('Лимит запросов')).toBeInTheDocument()
  expect(screen.getByText('Тайм-аут')).toBeInTheDocument()
  expect(screen.getByText('Сервис недоступен')).toBeInTheDocument()
  expect(screen.getByText('Ответ отклонён')).toBeInTheDocument()
  expect(screen.getByText('Некорректный ответ')).toBeInTheDocument()
  expect(screen.getByText('Некорректные ссылки на основания')).toBeInTheDocument()
  expect(screen.getByText('Небезопасный текст')).toBeInTheDocument()
  expect(screen.getByText('Неизвестная ошибка AI')).toBeInTheDocument()
  expect(screen.queryByText('private_provider_message')).not.toBeInTheDocument()
  expect(screen.queryByText('100')).not.toBeInTheDocument()
})

test('keeps diagnostics unchanged when explanation aggregates are absent', async () => {
  render(<DiagnosticsPage csrfToken="csrf" onDeleted={() => undefined} load={async () => ({
    status: 'ready', components: {}, jobs: [{ id: 'sync', kind: 'steam.sync', stage: '', status: 'succeeded', attempts: 1, createdAt: '', updatedAt: '' }],
  })} loadAdjustments={vi.fn().mockResolvedValue([])} />)
  expect(await screen.findByText('Ошибок фоновых заданий нет.')).toBeInTheDocument()
  expect(screen.queryByText('Объяснения рекомендаций')).not.toBeInTheDocument()
})
