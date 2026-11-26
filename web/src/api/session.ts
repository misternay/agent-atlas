const storageKey = 'atlas.session'

export function sessionSecret() {
  return globalThis.sessionStorage?.getItem(storageKey) ?? ''
}

export async function redeemSignIn() {
  const code = new URLSearchParams(location.hash.slice(1)).get('signin')
  if (!code) return
  history.replaceState(history.state, '', location.pathname + location.search)
  const response = await fetch('/api/v1/auth/exchange', {
    method: 'POST',
    credentials: 'omit',
    redirect: 'error',
    headers: { 'Content-Type': 'application/json', 'X-Atlas-CSRF': '1' },
    body: JSON.stringify({ code }),
  })
  if (response.ok) {
    const { session } = (await response.json()) as { session: string }
    sessionStorage.setItem(storageKey, session)
  }
}
