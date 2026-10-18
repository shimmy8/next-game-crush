import type { Recommendation } from '../api/types'
import { GamePoster } from './GamePoster'

export function canonicalSteamURL(item: Pick<Recommendation, 'steamAppId' | 'storeUrl'>) {
  return Number.isSafeInteger(item.steamAppId) && item.steamAppId > 0 && item.storeUrl === `https://store.steampowered.com/app/${item.steamAppId}/`
}
export function CompactGameIdentity({ item, size }: { item: Recommendation; size: 'grid' | 'list' }) {
  return <div className="compact-game-identity"><GamePoster name={item.name} coverImageUrl={item.coverImageUrl} size={size} decorative />
    {canonicalSteamURL(item) ? <a href={item.storeUrl} target="_blank" rel="noopener noreferrer" aria-label={`${item.name} — открыть в Steam, новая вкладка`}>{item.name}<span aria-hidden="true"> ↗</span></a> : <strong>{item.name}</strong>}
  </div>
}
