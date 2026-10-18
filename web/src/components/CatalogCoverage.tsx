import type { CatalogStatus } from '../api/types'

const coverageCopy: Record<CatalogStatus['status'], [string, string]> = {
  ready: ['Ограниченный обход каталога завершён', 'Рекомендации охватывают подходящие игры из текущего ограниченного обхода.'],
  backfilling: ['Каталог обновляется', 'Каталог может быть неполным: поиск новых игр продолжается.'],
  stale: ['Каталог давно не обновлялся', 'Последние сохранённые рекомендации остаются видны.'],
  unavailable: ['Обновление каталога недоступно', 'Последние сохранённые рекомендации остаются видны.'],
  disabled: ['Используется проверенный локальный каталог', 'Автоматическое расширение каталога отключено.'],
}

export function SourceTimestamp({ value }: { value: string | null }) {
  if (!value || !Number.isFinite(Date.parse(value))) return <>неизвестно</>
  return <time dateTime={value}>{new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(value))}</time>
}

export default function CatalogCoverage({ status }: { status: CatalogStatus }) {
  const [title, detail] = coverageCopy[status.status]
  return <aside className="windows-card catalog-coverage" aria-labelledby="catalog-coverage-title">
    <div className="eyebrow">Источники новых игр</div>
    <h2 id="catalog-coverage-title">Охват каталога</h2>
    <strong>{title}</strong>
    <p>{detail}</p>
    <p>Активных найденных игр: <strong>{status.activeDiscoveredGames}</strong></p>
    <p>Последнее обновление: <SourceTimestamp value={status.lastSuccessfulScanAt} /></p>
  </aside>
}
