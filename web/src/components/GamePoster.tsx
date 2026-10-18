import { useState } from 'react'

type Props = { name: string; coverImageUrl?: string; size: 'grid' | 'list'; decorative?: boolean }
export function GamePoster(props: Props) {
  return <PosterImage key={props.coverImageUrl ?? ''} {...props} />
}
function PosterImage({ name, coverImageUrl, size, decorative = false }: Props) {
  const [failed, setFailed] = useState(false)
  const width = size === 'grid' ? 32 : 48
  const height = size === 'grid' ? 48 : 72
  return coverImageUrl && !failed
    ? <img className={`game-poster game-poster--${size}`} src={coverImageUrl} width={width} height={height} loading="lazy" alt={decorative ? '' : `Обложка игры ${name}`} onError={() => setFailed(true)} />
    : <span className={`game-poster game-poster--${size} game-poster--fallback`} role={decorative ? undefined : 'img'} aria-hidden={decorative || undefined} aria-label={decorative ? undefined : `Обложка ${name} недоступна`}>{Array.from(name)[0]?.toLocaleUpperCase('ru') || '?'}</span>
}
