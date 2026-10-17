export type CurrentUser = {
  authenticated: true
  steamId: string
  csrfToken: string
  expiresAt: string
}

export async function getCurrentUser(signal?: AbortSignal): Promise<CurrentUser | null> {
  const response = await fetch('/api/me', { credentials: 'same-origin', signal })
  if (response.status === 401) return null
  if (!response.ok) throw new Error('Unable to load the signed-in user')
  return response.json() as Promise<CurrentUser>
}

export async function logout(csrfToken: string): Promise<void> {
  const response = await fetch('/auth/logout', {
    method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': csrfToken },
  })
  if (!response.ok) throw new Error('Unable to sign out')
}
