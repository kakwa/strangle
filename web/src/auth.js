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

// --- API token self-service --------------------------------------------------
//
// Tokens give user-linked CLIs and webhooks a bearer credential that
// authenticates /api requests as the owning user. The raw value is returned
// exactly once, at creation; only a display prefix is stored.

function apiError(res) {
  return res.json().catch(() => ({})).then((body) => body.error || `HTTP ${res.status}`)
}

// fetchTokens lists the caller's tokens. Throws with the server's message on
// 403/503 (no user account / no database), which the UI shows as a hint.
export async function fetchTokens() {
  const res = await fetch('/api/tokens')
  if (!res.ok) throw new Error(await apiError(res))
  return res.json()
}

// createToken returns { raw, token }; raw is the bearer value, shown once.
export async function createToken(name, ttlHours) {
  const res = await fetch('/api/tokens', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, ttl_hours: ttlHours || 0 }),
  })
  if (!res.ok) throw new Error(await apiError(res))
  return res.json()
}

// removeToken revokes one by name; a 404 (already gone) is not an error.
export async function removeToken(name) {
  const res = await fetch(`/api/tokens/${encodeURIComponent(name)}`, { method: 'DELETE' })
  if (!res.ok && res.status !== 404) throw new Error(await apiError(res))
}
