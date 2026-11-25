// Thin client for the UI authentication endpoints. All requests rely on the
// session cookie set by POST /api/login (HttpOnly, so never touch it from JS).

// fetchMe probes GET /api/me: it returns {authenticated, username?} when a
// session is valid or authentication is off, and null when authentication is
// on and the session is missing or expired (HTTP 401).
export async function fetchMe() {
  const res = await fetch('/api/me')
  if (res.status === 401) return null
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

// login authenticates against POST /api/login and returns the me response on
// success. Throws an Error carrying the server's message on 401.
export async function login(username, password) {
  const res = await fetch('/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error || `HTTP ${res.status}`)
  }
  return res.json()
}

// logout ends the session; failures are ignored (the cookie is cleared
// server-side, the UI just moves on to the login screen).
export async function logout() {
  try {
    await fetch('/api/logout', { method: 'POST' })
  } catch {
    // ignore
  }
}
