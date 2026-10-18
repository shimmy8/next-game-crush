import { useState } from 'react'

const avatarHosts = new Set([
  'avatars.steamstatic.com',
  'avatars.cloudflare.steamstatic.com',
  'avatars.fastly.steamstatic.com',
  'avatars.akamai.steamstatic.com',
])

function trustedAvatarUrl(value: string | null | undefined) {
  if (!value) return null
  try {
    const parsed = new URL(value)
    if (parsed.protocol !== 'https:' || parsed.username || parsed.password || parsed.port || !avatarHosts.has(parsed.hostname.toLowerCase())) return null
    return parsed.href
  } catch {
    return null
  }
}

export default function SteamIdentity({ displayName, avatarUrl, compact = false }: { displayName?: string | null; avatarUrl?: string | null; compact?: boolean }) {
  const safeAvatar = trustedAvatarUrl(avatarUrl)
  const [failedUrl, setFailedUrl] = useState<string | null>(null)
  const name = displayName?.trim() || 'Игрок Steam'

  return <span className={`steam-identity${compact ? ' steam-identity--compact' : ''}`}>
    {safeAvatar && failedUrl !== safeAvatar
      ? <img src={safeAvatar} alt={`Аватар ${name}`} onError={() => setFailedUrl(safeAvatar)} />
      : <span className="steam-identity__fallback" aria-label="Аватар Steam по умолчанию" aria-hidden={false}>S</span>}
    <span className="steam-identity__name">{name}</span>
  </span>
}
