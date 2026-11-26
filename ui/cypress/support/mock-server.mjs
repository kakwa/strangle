// Mock strangle API for the Cypress e2e suite.
//
// Serves the production build in dist/ (with SPA fallback) plus fixed fixture
// data for the versioned, org/collection-scoped API. There are deliberately no
// flat /api/* aliases: an unknown path answers 404, just like the real server
// after the API rework. Mock-only reset hooks under /api/v1/_mock/ restore
// stateful fixtures between tests. /metrics serves Prometheus text.

import { createServer } from 'node:http'
import { readFile, stat } from 'node:fs/promises'
import { randomBytes } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const dist = path.join(here, '..', '..', 'dist')
const PORT = process.env.MOCK_PORT ? Number(process.env.MOCK_PORT) : 18090

const now = Date.now()
const iso = (msAgo) => new Date(now - msAgo).toISOString()
const MIN = 60_000
const HOUR = 3_600_000
const V1 = '/api/v1'

let nextOrgID = 3

const PLAYBOOKS = {
  alpha: [
    {
      name: 'web', playbook: 'playbooks/web.yml', inventory: 'inventory/hosts',
      last_status: 'ok', last_run_at: iso(30 * MIN),
    },
    {
      name: 'common', playbook: 'playbooks/common.yml', inventory: 'inventory/hosts',
      last_status: 'ok', last_run_at: iso(3 * HOUR),
    },
  ],
  beta: [
    {
      name: 'databases', playbook: 'playbooks/db.yml', inventory: 'inventory/db',
      last_status: 'failed', last_run_at: iso(10 * MIN),
    },
    { name: 'bootstrap', playbook: 'playbooks/bootstrap.yml', inventory: 'inventory/db' },
  ],
}

const COLLECTIONS = [
  {
    id: 1, org: 'default', name: 'alpha', path: './ansible',
    playbooks: PLAYBOOKS.alpha.map((pb) => ({ name: pb.name })),
  },
  {
    id: 2, org: 'lab', name: 'beta', path: './edge-ansible',
    playbooks: PLAYBOOKS.beta.map((pb) => ({ name: pb.name })),
  },
]

// GET /api/v1/orgs already returns each org with its collections, matching the
// real handler. The UI discovers scope from this one response only.
let orgs = [
  { id: 1, name: 'default', collections: COLLECTIONS.filter((c) => c.org === 'default') },
  { id: 2, name: 'lab', collections: COLLECTIONS.filter((c) => c.org === 'lab') },
]

const HOSTS = {
  alpha: [
    {
      target: 'alpha', name: 'web-01', ip: '10.0.1.10', user: 'deploy',
      groups: ['webservers', 'prod'],
      in_inventory: true, active: true,
      first_seen_at: iso(24 * HOUR), last_seen_at: iso(30 * MIN), last_run_at: iso(30 * MIN),
      last_status: 'changed', last_change_at: iso(30 * MIN),
      facts: {
        ansible_hostname: 'web-01',
        ansible_os_family: 'Debian',
        ansible_default_ipv4: { address: '10.0.1.10' },
      },
    },
    {
      target: 'alpha', name: 'web-02', ip: '10.0.1.11', user: 'deploy',
      groups: ['webservers'],
      in_inventory: true, active: true,
      first_seen_at: iso(24 * HOUR), last_seen_at: iso(HOUR), last_run_at: iso(HOUR),
      last_status: 'ok',
    },
    {
      target: 'alpha', name: 'retired-01', ip: '10.0.1.99',
      groups: ['webservers'],
      in_inventory: true, active: false,
      first_seen_at: iso(48 * HOUR), last_seen_at: iso(2 * HOUR), last_run_at: iso(2 * HOUR),
      last_status: 'ok', disappeared_at: iso(2 * HOUR),
    },
  ],
  beta: [
    {
      target: 'beta', name: 'db-01', ip: '10.0.2.20',
      groups: ['databases'],
      in_inventory: true, active: true,
      first_seen_at: iso(24 * HOUR), last_seen_at: iso(10 * MIN), last_run_at: iso(10 * MIN),
      last_status: 'failed',
    },
    {
      // A host that answered but is not in any tracked inventory.
      target: 'beta', name: 'adhoc-01',
      in_inventory: false, active: true,
      first_seen_at: iso(HOUR), last_seen_at: iso(20 * MIN), last_run_at: iso(20 * MIN),
      last_status: 'changed', last_change_at: iso(20 * MIN),
    },
  ],
}

// Runs are keyed by org/collection/playbook/host: there is no global host-run
// endpoint anymore.
const HOST_RUNS = {
  'default/alpha/web/web-01': [
    {
      run_id: 3, host: 'web-01', playbook: 'web',
      started_at: iso(30 * MIN), duration_ms: 3800, exit_code: 0, success: true,
      status: 'changed', ok: 12, changed: 3, failed: 0, unreachable: 0, skipped: 1,
      rescued: 0, ignored: 0,
    },
  ],
  'default/alpha/common/web-01': [
    {
      run_id: 1, host: 'web-01', playbook: 'common',
      started_at: iso(3 * HOUR), duration_ms: 2100, exit_code: 0, success: true,
      status: 'ok', ok: 8, changed: 0, failed: 0, unreachable: 0, skipped: 0,
      rescued: 0, ignored: 0,
    },
  ],
  'default/alpha/web/web-02': [
    {
      run_id: 3, host: 'web-02', playbook: 'web',
      started_at: iso(30 * MIN), duration_ms: 3800, exit_code: 0, success: true,
      status: 'ok', ok: 7, changed: 0, failed: 0, unreachable: 0, skipped: 0,
      rescued: 0, ignored: 0,
    },
  ],
  'lab/beta/databases/db-01': [
    {
      run_id: 4, host: 'db-01', playbook: 'databases',
      started_at: iso(10 * MIN), duration_ms: 5000, exit_code: 2, success: false,
      status: 'failed', ok: 4, changed: 0, failed: 2, unreachable: 0, skipped: 5,
      rescued: 0, ignored: 0,
    },
  ],
  'lab/beta/bootstrap/db-01': [
    {
      run_id: 2, host: 'db-01', playbook: 'bootstrap',
      started_at: iso(2 * HOUR), duration_ms: 9000, exit_code: 0, success: true,
      status: 'changed', ok: 5, changed: 1, failed: 0, unreachable: 0, skipped: 0,
      rescued: 0, ignored: 0,
    },
  ],
}

const HOSTKEY_FIXTURES = [
  {
    org: 'default', collection: 'alpha', playbook: 'web', host: '10.0.1.10',
    fingerprint: 'SHA256:mock-alpha-key',
    public_key: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAlpha',
    status: 'pending', first_seen: iso(5 * MIN),
  },
  {
    org: 'lab', collection: 'beta', playbook: 'databases', host: '10.0.2.20',
    fingerprint: 'SHA256:mock-beta-key',
    public_key: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBeta',
    status: 'pending', first_seen: iso(HOUR),
  },
]
let hostkeys = HOSTKEY_FIXTURES.map((k) => ({ ...k }))

const WORKERS_FIXTURE = [
  { worker: '0', collection: 'alpha', jobs: 1, restarts: 3, alive: true },
  { worker: '1', collection: 'beta', jobs: 0, restarts: 0, alive: false },
]
let workers = WORKERS_FIXTURE.map((w) => ({ ...w }))

function collectionExists(org, collection) {
  return COLLECTIONS.some((c) => c.org === org && c.name === collection)
}

function collectionDetail(org, collection) {
  const found = COLLECTIONS.find((c) => c.org === org && c.name === collection)
  if (!found) return null
  return {
    ...found,
    playbooks: PLAYBOOKS[collection] || found.playbooks,
  }
}

function metricsText() {
  const alive = workers.filter((w) => w.alive).length
  const dead = workers.length - alive
  const lines = [
    '# HELP strangle_workers Number of worker slots by state.',
    '# TYPE strangle_workers gauge',
    `strangle_workers{state="total"} ${workers.length}`,
    `strangle_workers{state="alive"} ${alive}`,
    `strangle_workers{state="dead"} ${dead}`,
    '# HELP strangle_worker_jobs In-flight jobs assigned to each worker slot.',
    '# TYPE strangle_worker_jobs gauge',
  ]
  for (const w of workers) {
    lines.push(`strangle_worker_jobs{worker="${w.worker}",collection="${w.collection}"} ${w.jobs}`)
  }
  lines.push('# HELP strangle_worker_restarts_total Process restarts of each worker slot.')
  lines.push('# TYPE strangle_worker_restarts_total counter')
  for (const w of workers) {
    lines.push(`strangle_worker_restarts_total{worker="${w.worker}",collection="${w.collection}"} ${w.restarts}`)
  }
  return `${lines.join('\n')}\n`
}

// --- auth ------------------------------------------------------------------
//
// With a user configured, every /api/v1/* request needs a valid session
// cookie except health, login, logout and me. The single account is
// admin/secret. User tokens are scoped to one org/collection.

const SESSION_COOKIE = 'strangle_session'
const USERS = { admin: 'secret' }
const SESSION_TTL_MS = 12 * HOUR
const sessions = new Map() // token -> { username, expires }
const TOKEN_PREFIX = 'st_'
const tokens = new Map() // raw -> { name, owner, org, collection, prefix, ... }

function parseCookies(header) {
  const out = {}
  for (const part of (header || '').split(';')) {
    const i = part.indexOf('=')
    if (i > 0) out[part.slice(0, i).trim()] = decodeURIComponent(part.slice(i + 1).trim())
  }
  return out
}

function sessionOf(req) {
  const tok = parseCookies(req.headers.cookie)[SESSION_COOKIE]
  if (!tok || !sessions.has(tok)) return null
  const s = sessions.get(tok)
  if (Date.now() > s.expires) {
    sessions.delete(tok)
    return null
  }
  return s
}

function bearerOf(req) {
  const auth = req.headers.authorization || ''
  const x = req.headers['x-strangle-token']
  const header = x && !auth ? `Bearer ${x}` : auth
  const m = header.match(/^Bearer (.+)$/)
  return m ? m[1] : null
}

function identityOf(req) {
  const me = sessionOf(req)
  if (me) return { username: me.username }
  const raw = bearerOf(req)
  if (raw && tokens.has(raw)) {
    const t = tokens.get(raw)
    if (!t.expires_at || Date.now() < new Date(t.expires_at).getTime()) {
      t.last_used_at = new Date().toISOString()
      return { username: t.owner }
    }
  }
  return null
}

function json(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(body))
}

function readJSON(req, res, handle) {
  let body = ''
  req.on('data', (c) => (body += c))
  req.on('end', () => {
    let value
    try {
      value = JSON.parse(body || '{}')
    } catch {
      return json(res, 400, { error: 'invalid JSON' })
    }
    return handle(value)
  })
}

function publicToken({ owner, ...rest }) {
  return rest
}

// --- host keys -------------------------------------------------------------

function patchHostKeys(req, res, org, collection, playbook, host) {
  readJSON(req, res, (patch) => {
    if (!Array.isArray(patch)) return json(res, 400, { error: 'JSON Patch array required' })
    const key = hostkeys.find((k) =>
      k.org === org && k.collection === collection && k.playbook === playbook && k.host === host,
    )
    if (!key) return json(res, 404, { error: `no host key for ${playbook}:${host}` })
    let status = key.status
    let fingerprint = key.fingerprint
    for (const op of patch) {
      if (op?.op !== 'replace') return json(res, 400, { error: `unsupported operation ${op?.op}` })
      if (op.path === '/status') {
        if (op.value !== 'approved' && op.value !== 'rejected') {
          return json(res, 400, { error: 'status must be approved or rejected' })
        }
        status = op.value
      } else if (op.path === '/fingerprint') {
        if (typeof op.value !== 'string' || !op.value) {
          return json(res, 400, { error: 'fingerprint must be a string' })
        }
        fingerprint = op.value
      } else {
        return json(res, 400, { error: `unknown path ${op.path}` })
      }
    }
    key.status = status
    key.fingerprint = fingerprint
    return json(res, 200, { status, host })
  })
}

// --- API -------------------------------------------------------------------

function handleApi(req, res, url) {
  const p = url.pathname

  if (req.method === 'GET' && p === `${V1}/health`) {
    return json(res, 200, { status: 'ok' })
  }
  if (req.method === 'GET' && p === `${V1}/openapi.json`) {
    return json(res, 200, { openapi: '3.1.0', info: { title: 'strangle', version: '1' }, paths: {} })
  }
  if (req.method === 'POST' && p === `${V1}/login`) {
    return readJSON(req, res, (creds) => {
      if (typeof creds.username !== 'string' || USERS[creds.username] !== creds.password) {
        return json(res, 401, { error: 'invalid username or password' })
      }
      const token = randomBytes(32).toString('hex')
      sessions.set(token, { username: creds.username, expires: Date.now() + SESSION_TTL_MS })
      res.writeHead(200, {
        'Content-Type': 'application/json',
        'Set-Cookie': `${SESSION_COOKIE}=${token}; Path=/; HttpOnly; SameSite=Lax; Max-Age=43200`,
      })
      return res.end(JSON.stringify({ authenticated: true, username: creds.username }))
    })
  }
  if (req.method === 'POST' && p === `${V1}/logout`) {
    const tok = parseCookies(req.headers.cookie)[SESSION_COOKIE]
    if (tok) sessions.delete(tok)
    res.writeHead(200, {
      'Content-Type': 'application/json',
      'Set-Cookie': `${SESSION_COOKIE}=; Path=/; Max-Age=0`,
    })
    return res.end(JSON.stringify({ status: 'logged out' }))
  }
  if (req.method === 'GET' && p === `${V1}/me`) {
    const me = identityOf(req)
    if (!me) return json(res, 401, { error: 'not authenticated' })
    return json(res, 200, { authenticated: true, username: me.username })
  }

  const me = identityOf(req)
  if (!me) return json(res, 401, { error: 'not authenticated' })

  // Mock-only hooks for stateful fixtures.
  if (p.startsWith(`${V1}/_mock/`)) {
    if (req.method === 'POST' && p === `${V1}/_mock/reset-hostkeys`) {
      hostkeys = HOSTKEY_FIXTURES.map((k) => ({ ...k }))
      return json(res, 200, { status: 'reset' })
    }
    if (req.method === 'POST' && p === `${V1}/_mock/reset-tokens`) {
      for (const [raw, t] of tokens) if (t.owner === me.username) tokens.delete(raw)
      return json(res, 200, { status: 'reset' })
    }
    if (req.method === 'POST' && p === `${V1}/_mock/reset-workers`) {
      workers = WORKERS_FIXTURE.map((w) => ({ ...w }))
      return json(res, 200, { status: 'reset' })
    }
    if (req.method === 'POST' && p === `${V1}/_mock/set-workers`) {
      return readJSON(req, res, (body) => {
        workers = Array.isArray(body.workers)
          ? body.workers.map((w) => ({
            worker: String(w.worker ?? ''),
            collection: String(w.collection ?? ''),
            jobs: Number(w.jobs) || 0,
            restarts: Number(w.restarts) || 0,
            alive: Boolean(w.alive),
          }))
          : []
        return json(res, 200, { status: 'updated' })
      })
    }
    return json(res, 404, { error: `unknown mock hook ${p}` })
  }

  // Org directory (orgs are independent records; collections are a subresource).
  if (req.method === 'GET' && p === `${V1}/orgs`) {
    return json(res, 200, orgs)
  }
  if (req.method === 'POST' && p === `${V1}/orgs`) {
    return readJSON(req, res, (body) => {
      const name = typeof body.name === 'string' ? body.name.trim() : ''
      if (!name) return json(res, 400, { error: 'name is required' })
      if (orgs.some((o) => o.name === name)) {
        return json(res, 409, { error: `org "${name}" already exists` })
      }
      const rec = { id: nextOrgID++, name, collections: [] }
      orgs = [...orgs, rec]
      return json(res, 201, rec)
    })
  }
  const orgDetail = p.match(/^\/api\/v1\/orgs\/([^/]+)$/)
  if (req.method === 'GET' && orgDetail) {
    const org = decodeURIComponent(orgDetail[1])
    const found = orgs.find((o) => o.name === org)
    if (!found) return json(res, 404, { error: `unknown org ${org}` })
    return json(res, 200, found)
  }
  const orgCollections = p.match(/^\/api\/v1\/orgs\/([^/]+)\/collections$/)
  if (req.method === 'GET' && orgCollections) {
    const org = decodeURIComponent(orgCollections[1])
    if (!orgs.some((o) => o.name === org)) return json(res, 404, { error: `unknown org ${org}` })
    return json(res, 200, COLLECTIONS.filter((c) => c.org === org))
  }

  const coll = p.match(/^\/api\/v1\/orgs\/([^/]+)\/collections\/([^/]+)$/)
  if (req.method === 'GET' && coll) {
    const org = decodeURIComponent(coll[1])
    const collection = decodeURIComponent(coll[2])
    const detail = collectionDetail(org, collection)
    if (!detail) return json(res, 404, { error: `unknown collection ${org}/${collection}` })
    return json(res, 200, detail)
  }

  // Playbook/host-scoped runs and host keys (deeper paths than the generic
  // collection subresources below).
  const hostResource = p.match(
    /^\/api\/v1\/orgs\/([^/]+)\/collections\/([^/]+)\/playbooks\/([^/]+)\/hosts\/([^/]+)\/(runs|hostkeys)$/,
  )
  if (hostResource) {
    if (!collectionExists(decodeURIComponent(hostResource[1]), decodeURIComponent(hostResource[2]))) {
      return json(res, 404, { error: 'unknown collection' })
    }
    const hOrg = decodeURIComponent(hostResource[1])
    const hColl = decodeURIComponent(hostResource[2])
    const playbook = decodeURIComponent(hostResource[3])
    const host = decodeURIComponent(hostResource[4])
    const kind = hostResource[5]
    if (req.method === 'GET' && kind === 'runs') {
      const limit = Number(url.searchParams.get('limit') || 10)
      const key = `${hOrg}/${hColl}/${playbook}/${host}`
      return json(res, 200, (HOST_RUNS[key] || []).slice(0, limit))
    }
    if (kind === 'hostkeys') {
      if (req.method === 'GET') {
        return json(res, 200, hostkeys.filter((k) =>
          k.org === hOrg && k.collection === hColl && k.playbook === playbook && k.host === host,
        ).map(({ org: _o, collection: _c, ...pub }) => pub))
      }
      if (req.method === 'PATCH') {
        return patchHostKeys(req, res, hOrg, hColl, playbook, host)
      }
    }
  }

  const sub = p.match(/^\/api\/v1\/orgs\/([^/]+)\/collections\/([^/]+)\/([^/]+(?:\/[^/]+)?)$/)
  if (sub) {
    const org = decodeURIComponent(sub[1])
    const collection = decodeURIComponent(sub[2])
    const resource = sub[3]
    if (!collectionExists(org, collection)) {
      return json(res, 404, { error: `unknown collection ${org}/${collection}` })
    }

    if (req.method === 'GET' && resource === 'hosts') {
      return json(res, 200, HOSTS[collection] || [])
    }
    if (req.method === 'GET' && resource === 'playbooks') {
      return json(res, 200, PLAYBOOKS[collection] || [])
    }
    if (req.method === 'POST' && resource === 'hook') {
      return json(res, 200, { status: 'ok' })
    }

    // Collection-scoped API tokens.
    if (resource === 'tokens') {
      if (req.method === 'GET') {
        const mine = [...tokens.values()]
          .filter((t) => t.owner === me.username && t.org === org && t.collection === collection
            && (!t.expires_at || Date.now() < new Date(t.expires_at).getTime()))
          .sort((a, b) => a.name.localeCompare(b.name))
        return json(res, 200, mine.map(publicToken))
      }
      if (req.method === 'POST') {
        return readJSON(req, res, (body) => {
          const name = typeof body.name === 'string' ? body.name.trim() : ''
          if (!name) return json(res, 400, { error: 'name is required' })
          const dup = [...tokens.values()].some((t) =>
            t.owner === me.username && t.org === org && t.collection === collection && t.name === name,
          )
          if (dup) return json(res, 409, { error: `token "${name}" already exists` })
          const raw = TOKEN_PREFIX + randomBytes(32).toString('hex')
          const rec = {
            name, owner: me.username, org, collection,
            prefix: raw.slice(0, TOKEN_PREFIX.length + 8),
            created_at: new Date().toISOString(), last_used_at: null,
          }
          const ttl = Number(body.ttl_hours) || 0
          if (ttl > 0) rec.expires_at = new Date(Date.now() + ttl * HOUR).toISOString()
          tokens.set(raw, rec)
          return json(res, 201, { raw, token: publicToken(rec) })
        })
      }
    }
    const tokenDel = resource.match(/^tokens\/([^/]+)$/)
    if (req.method === 'DELETE' && tokenDel) {
      const name = decodeURIComponent(tokenDel[1])
      for (const [raw, t] of tokens) {
        if (t.owner === me.username && t.org === org && t.collection === collection && t.name === name) {
          tokens.delete(raw)
          res.writeHead(204)
          return res.end()
        }
      }
      return json(res, 404, { error: `token "${name}" not found` })
    }
  }

  return json(res, 404, { error: `unknown API route ${p}` })
}

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.json': 'application/json',
  '.map': 'application/json',
  '.woff2': 'font/woff2',
}

async function serveStatic(req, res, url) {
  let file = path.normalize(path.join(dist, decodeURIComponent(url.pathname)))
  if (!file.startsWith(dist)) {
    res.writeHead(403)
    return res.end()
  }
  try {
    const st = await stat(file)
    if (st.isDirectory()) file = path.join(file, 'index.html')
  } catch {
    // SPA fallback: unknown paths (e.g. /hosts/web-01) serve the app shell.
    file = path.join(dist, 'index.html')
  }
  try {
    const data = await readFile(file)
    res.writeHead(200, { 'Content-Type': TYPES[path.extname(file)] || 'application/octet-stream' })
    res.end(data)
  } catch {
    res.writeHead(404)
    res.end('not found')
  }
}

createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${PORT}`)
  if (url.pathname === '/metrics') {
    res.writeHead(200, { 'Content-Type': 'text/plain; version=0.0.4; charset=utf-8' })
    return res.end(metricsText())
  }
  if (url.pathname.startsWith(`${V1}/`)) {
    return handleApi(req, res, url)
  }
  // Flat /api/* aliases were removed entirely; never serve the SPA shell for
  // a request that looks like an old API route.
  if (url.pathname.startsWith('/api/')) {
    res.writeHead(404, { 'Content-Type': 'application/json' })
    return res.end(JSON.stringify({ error: `unknown API route ${url.pathname}` }))
  }
  return serveStatic(req, res, url)
}).listen(PORT, '127.0.0.1', () => {
  console.log(`mock strangle server on http://127.0.0.1:${PORT} (serving ${dist})`)
})
