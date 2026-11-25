// Mock strangle API for the Cypress e2e suite.
//
// Serves the production build in dist/ (with SPA fallback) plus a fixed set
// of fixture data whose JSON shapes match the real API exactly (see
// internal/server/server.go and internal/store/store.go). POST /api/run is
// stateful: it appends a new run to /api/runs so the UI's pending-run
// tracking can be exercised end to end. Host-key decisions are stateful too;
// /api/_mock/reset-hostkeys (mock-only) restores the fixtures between tests.
// /api/v1/... paths are normalized to their flat aliases so the UI can use
// the canonical versioned endpoints against this server.

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

const HOSTS = [
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
    target: 'beta', name: 'db-01', ip: '10.0.2.20',
    groups: ['databases'],
    in_inventory: true, active: true,
    first_seen_at: iso(24 * HOUR), last_seen_at: iso(10 * MIN), last_run_at: iso(10 * MIN),
    last_status: 'failed',
  },
  {
    target: 'alpha', name: 'retired-01', ip: '10.0.1.99',
    groups: ['webservers'],
    in_inventory: true, active: false,
    first_seen_at: iso(48 * HOUR), last_seen_at: iso(2 * HOUR), last_run_at: iso(2 * HOUR),
    last_status: 'ok', disappeared_at: iso(2 * HOUR),
  },
  {
    // A host that answered but is not in any tracked (directory) inventory.
    target: 'beta', name: 'adhoc-01',
    in_inventory: false, active: true,
    first_seen_at: iso(HOUR), last_seen_at: iso(20 * MIN), last_run_at: iso(20 * MIN),
    last_status: 'changed', last_change_at: iso(20 * MIN),
  },
]

let runs = [
  {
    id: 4, target: 'beta', playbook: 'databases',
    started_at: iso(10 * MIN), finished_at: iso(10 * MIN - 5_000), duration_ms: 5000,
    exit_code: 2, success: false, hosts: 2, changed_hosts: 0, failed_hosts: 1,
  },
  {
    id: 3, target: 'alpha', playbook: 'web',
    started_at: iso(30 * MIN), finished_at: iso(30 * MIN - 3800), duration_ms: 3800,
    exit_code: 0, success: true, hosts: 2, changed_hosts: 1, failed_hosts: 0,
  },
  {
    id: 2, target: 'beta', playbook: 'bootstrap',
    started_at: iso(2 * HOUR), finished_at: iso(2 * HOUR - 9_000), duration_ms: 9000,
    exit_code: 0, success: true, hosts: 1, changed_hosts: 1, failed_hosts: 0,
  },
  {
    id: 1, target: 'alpha', playbook: 'common',
    started_at: iso(3 * HOUR), finished_at: iso(3 * HOUR - 2100), duration_ms: 2100,
    exit_code: 0, success: true, hosts: 2, changed_hosts: 0, failed_hosts: 0,
  },
]
const RUNS_FIXTURES = runs.map((r) => ({ ...r }))
let nextRunID = 5

// Full logs are only returned by the detail endpoint, just like the real API.
const OUTPUTS = {
  4: 'PLAY [databases] ***\nTASK [Restore backup] ***\nfatal: connection refused\n',
  3: 'PLAY [web] ***\nTASK [Update Nginx config] ***\nchanged: [web-01]\n',
  2: 'PLAY [bootstrap] ***\nTASK [Install packages] ***\nok: [db-01]\n',
  1: 'PLAY [common] ***\nTASK [Set timezone] ***\nok: [web-01]\n',
}

const STATS = {
  hosts: { total: 5, in_inventory: 4, active: 4, disappeared: 1 },
  runs: { total: 4, succeeded: 3, failed: 1, avg_duration_ms: 4975, last_run_at: iso(10 * MIN) },
  per_playbook: [
    { target: 'alpha', playbook: 'web', total: 3, failed: 0, last_run_at: iso(30 * MIN) },
    { target: 'alpha', playbook: 'common', total: 2, failed: 0, last_run_at: iso(3 * HOUR) },
    { target: 'beta', playbook: 'databases', total: 2, failed: 1, last_run_at: iso(10 * MIN) },
    { target: 'beta', playbook: 'bootstrap', total: 1, failed: 0, last_run_at: iso(2 * HOUR) },
  ],
}

const HOST_RUNS = {
  'web-01': [
    {
      run_id: 3, target: 'alpha', host: 'web-01', playbook: 'web',
      started_at: iso(30 * MIN), duration_ms: 3800, exit_code: 0, success: true,
      status: 'changed', ok: 12, changed: 3, failed: 0, unreachable: 0, skipped: 1,
      rescued: 0, ignored: 0,
    },
    {
      run_id: 1, target: 'alpha', host: 'web-01', playbook: 'common',
      started_at: iso(3 * HOUR), duration_ms: 2100, exit_code: 0, success: true,
      status: 'ok', ok: 8, changed: 0, failed: 0, unreachable: 0, skipped: 0,
      rescued: 0, ignored: 0,
    },
  ],
  'db-01': [
    {
      run_id: 4, target: 'beta', host: 'db-01', playbook: 'databases',
      started_at: iso(10 * MIN), duration_ms: 5000, exit_code: 2, success: false,
      status: 'failed', ok: 4, changed: 0, failed: 2, unreachable: 0, skipped: 5,
      rescued: 0, ignored: 0,
    },
  ],
}

const HOST_CHANGES = {
  'web-01': [
    {
      target: 'alpha', host: 'web-01', task: 'Update Nginx config', status: 'changed',
      last_run_id: 3, last_run_at: iso(30 * MIN), last_changed_at: iso(30 * MIN),
      last_diff:
        '--- before\n' +
        '+++ after: /etc/nginx/sites-available/default\n' +
        '@@ -1,4 +1,5 @@\n' +
        ' server {\n' +
        '-    server_name old.example.com;\n' +
        '+    server_name web01.example.com;\n' +
        '+    listen 8443 ssl;\n' +
        '     root /var/www/html;\n',
    },
    {
      target: 'alpha', host: 'web-01', task: 'Ensure Nginx is running', status: 'ok',
      last_run_id: 3, last_run_at: iso(30 * MIN),
    },
    {
      target: 'alpha', host: 'web-01', task: 'Collect disk usage', status: 'failed',
      last_run_id: 3, last_run_at: iso(30 * MIN),
      last_error: 'fatal: [web-01]: FAILED! => {"msg": "df: command not found"}\n',
    },
  ],
  'db-01': [
    {
      target: 'beta', host: 'db-01', task: 'Restore backup', status: 'failed',
      last_run_id: 4, last_run_at: iso(10 * MIN),
      last_error: 'fatal: [db-01]: FAILED! => {"msg": "pg_restore: connection to server failed"}\n',
    },
  ],
}

// Per-run task history is append-only: unlike HOST_CHANGES (latest state),
// this endpoint keeps every task result of each run.
const RUN_TASKS = {}
let nextTaskID = 1
for (const changes of Object.values(HOST_CHANGES)) {
  for (const c of changes) {
    const key = String(c.last_run_id)
    if (!RUN_TASKS[key]) RUN_TASKS[key] = []
    RUN_TASKS[key].push({
      id: nextTaskID++, run_id: c.last_run_id, target: c.target, host: c.host,
      task: c.task, status: c.status, diff: c.last_diff || '', error: c.last_error || '',
      finished_at: c.last_run_at,
    })
  }
}

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

// --- SSH administration (mirrors internal/server) --------------------------

const ORGS = [
  {
    name: 'default',
    collections: [
      { name: 'alpha', org: 'default', playbooks: PLAYBOOKS.alpha.map((pb) => ({ name: pb.name })) },
    ],
  },
  {
    name: 'lab',
    collections: [
      { name: 'beta', org: 'lab', playbooks: PLAYBOOKS.beta.map((pb) => ({ name: pb.name })) },
    ],
  },
]

const SSH_STATS = {
  'alpha/web': { active_connections: 2, total_dials: 7 },
  'beta/databases': { active_connections: 0, total_dials: 3 },
}

const HOSTKEY_FIXTURES = [
  {
    target: 'alpha/web', host: '10.0.1.10',
    fingerprint: 'SHA256:mock-alpha-key',
    public_key: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAlpha',
    status: 'pending', first_seen: iso(5 * MIN),
  },
  {
    target: 'beta/databases', host: '10.0.2.20',
    fingerprint: 'SHA256:mock-beta-key',
    public_key: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBeta',
    status: 'rejected', first_seen: iso(HOUR),
  },
]
let hostkeys = HOSTKEY_FIXTURES.map((k) => ({ ...k }))

function collectionExists(org, collection) {
  return ORGS.some((o) => o.name === org && o.collections.some((c) => c.name === collection))
}

function decideHostKey(req, res, kind, collection) {
  let body = ''
  req.on('data', (c) => (body += c))
  req.on('end', () => {
    let req2
    try {
      req2 = JSON.parse(body || '{}')
    } catch {
      return json(res, 400, { error: 'invalid JSON' })
    }
    const scope = req2.target
    if (!scope) return json(res, 400, { error: 'target scope is required' })
    if (collection && scope.split('/')[0] !== collection) {
      return json(res, 400, { error: `scope ${scope} is not in collection ${collection}` })
    }
    const i = hostkeys.findIndex((k) => k.target === scope && k.host === req2.host)
    if (i < 0) return json(res, 400, { error: `no pending host key for ${scope}:${req2.host}` })
    if (kind === 'accept' && req2.fingerprint && hostkeys[i].fingerprint !== req2.fingerprint) {
      return json(res, 400, { error: 'fingerprint mismatch' })
    }
    hostkeys.splice(i, 1)
    return json(res, 200, { status: kind === 'accept' ? 'accepted' : 'rejected', host: req2.host })
  })
}

// --- auth (mirrors internal/server) ----------------------------------------
//
// The mock enforces the same contract as the real server: with a user
// configured, every /api/* request needs a valid session cookie, except
// health, login, logout and me. The single account is admin/secret.

const SESSION_COOKIE = 'strangle_session'
const USERS = { admin: 'secret' }
const SESSION_TTL_MS = 12 * HOUR
const sessions = new Map() // token -> { username, expires }

// User API tokens (mirrors the /api/tokens self-service). The raw value is
// kept only for the mock's lifetime; the real server stores just its hash.
const TOKEN_PREFIX = 'st_'
const tokens = new Map() // raw -> { name, owner, prefix, created_at, expires_at?, last_used_at? }

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

// bearerOf extracts a bearer credential, via Authorization or
// X-Strangle-Token, mirroring the real server's extraction rules.
function bearerOf(req) {
  const auth = req.headers.authorization || ''
  const x = req.headers['x-strangle-token']
  const header = x && !auth ? `Bearer ${x}` : auth
  const m = header.match(/^Bearer (.+)$/)
  return m ? m[1] : null
}

// identityOf resolves who a request acts as: a session or a user token.
// Returns { username } or null.
function identityOf(req) {
  const me = sessionOf(req)
  if (me) return { username: me.username }
  const raw = bearerOf(req)
  if (raw && tokens.has(raw)) {
    const t = tokens.get(raw)
    if (!t.expires_at || Date.now() < new Date(t.expires_at).getTime()) {
      const now = new Date().toISOString()
      t.last_used_at = now
      return { username: t.owner }
    }
  }
  return null
}

function json(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(body))
}

function handleApi(req, res, url) {
  // The UI uses canonical /api/v1 paths; normalize them to the flat aliases
  // handled below. Org/collection subresources are matched separately.
  const p = url.pathname.replace(/^\/api\/v\d+\//, '/api/')
  if (req.method === 'GET' && p === '/api/health') {
    return json(res, 200, { status: 'ok' })
  }
  // Mock-only test hook: host-key decisions are stateful, so each spec can
  // restore the fixtures without restarting the server.
  if (req.method === 'POST' && p === '/api/_mock/reset-hostkeys') {
    hostkeys = HOSTKEY_FIXTURES.map((k) => ({ ...k }))
    return json(res, 200, { status: 'reset' })
  }
  if (req.method === 'POST' && p === '/api/_mock/reset-runs-base') {
    runs = RUNS_FIXTURES.map((r) => ({ ...r }))
    nextRunID = 5
    return json(res, 200, { status: 'reset' })
  }
  if (req.method === 'POST' && p === '/api/_mock/reset-runs') {
    runs = RUNS_FIXTURES.map((r) => ({ ...r }))
    runs = [{
      id: 5, target: 'alpha', playbook: 'web',
      started_at: iso(5_000), finished_at: '0001-01-01T00:00:00Z', duration_ms: 0,
      exit_code: 0, success: false, hosts: 0, changed_hosts: 0, failed_hosts: 0,
      state: 'running', canceled: false, job_id: '42',
      output: 'PLAY [all] ***\nrunning\n',
    }, ...runs]
    nextRunID = 6
    return json(res, 200, { status: 'reset' })
  }
  if (req.method === 'POST' && p === '/api/login') {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      let creds
      try {
        creds = JSON.parse(body || '{}')
      } catch {
        return json(res, 400, { error: 'invalid JSON' })
      }
      if (typeof creds.username !== 'string' || USERS[creds.username] !== creds.password) {
        return json(res, 401, { error: 'invalid username or password' })
      }
      const token = randomBytes(32).toString('hex')
      sessions.set(token, { username: creds.username, expires: Date.now() + SESSION_TTL_MS })
      res.writeHead(200, {
        'Content-Type': 'application/json',
        'Set-Cookie': `${SESSION_COOKIE}=${token}; Path=/; HttpOnly; SameSite=Lax; Max-Age=43200`,
      })
      res.end(JSON.stringify({ authenticated: true, username: creds.username }))
    })
    return
  }
  if (req.method === 'POST' && p === '/api/logout') {
    const tok = parseCookies(req.headers.cookie)[SESSION_COOKIE]
    if (tok) sessions.delete(tok)
    res.writeHead(200, {
      'Content-Type': 'application/json',
      'Set-Cookie': `${SESSION_COOKIE}=; Path=/; Max-Age=0`,
    })
    res.end(JSON.stringify({ status: 'logged out' }))
    return
  }
  const me = identityOf(req)
  if (req.method === 'GET' && p === '/api/me') {
    if (!me) return json(res, 401, { error: 'not authenticated' })
    return json(res, 200, { authenticated: true, username: me.username })
  }
  if (!me) {
    return json(res, 401, { error: 'not authenticated' })
  }
  if (req.method === 'GET' && p === '/api/stats') {
    return json(res, 200, STATS)
  }
  if (req.method === 'GET' && p === '/api/runs') {
    const limit = Number(url.searchParams.get('limit') || 50)
    return json(res, 200, runs.slice(0, limit))
  }
  const runDetail = p.match(/^\/api\/runs\/(\d+)$/)
  if (req.method === 'GET' && runDetail) {
    const run = runs.find((r) => String(r.id) === runDetail[1])
    if (!run) return json(res, 404, { error: `run ${runDetail[1]} not found` })
    return json(res, 200, { ...run, output: OUTPUTS[run.id] ?? run.output ?? '' })
  }
  if (req.method === 'GET' && p === '/api/hosts') {
    return json(res, 200, HOSTS)
  }
  const hostRuns = p.match(/^\/api\/hosts\/([^/]+)\/runs$/)
  if (req.method === 'GET' && hostRuns) {
    const host = decodeURIComponent(hostRuns[1])
    return json(res, 200, HOST_RUNS[host] || [])
  }
  const hostChanges = p.match(/^\/api\/hosts\/([^/]+)\/changes$/)
  if (req.method === 'GET' && hostChanges) {
    const host = decodeURIComponent(hostChanges[1])
    return json(res, 200, HOST_CHANGES[host] || [])
  }
  if (req.method === 'GET' && p === '/api/playbooks') {
    return json(res, 200, PLAYBOOKS)
  }
  if (req.method === 'GET' && p === '/api/orgs') {
    return json(res, 200, ORGS)
  }
  const runCancel = p.match(/^\/api\/orgs\/([^/]+)\/collections\/([^/]+)\/runs\/(\d+)\/cancel$/)
  if (req.method === 'POST' && runCancel) {
    const org = decodeURIComponent(runCancel[1])
    const coll = decodeURIComponent(runCancel[2])
    if (!collectionExists(org, coll)) return json(res, 404, { error: `unknown collection ${org}/${coll}` })
    const run = runs.find((r) => String(r.id) === runCancel[3])
    if (!run || run.target !== coll) return json(res, 404, { error: `run ${runCancel[3]} not found` })
    if (run.state !== 'queued' && run.state !== 'running') {
      return json(res, 409, { error: 'run is not active' })
    }
    run.state = 'cancelled'
    run.canceled = true
    run.finished_at = new Date().toISOString()
    run.output = `${run.output || ''}\n[strangle] run canceled`
    return json(res, 200, { id: run.id, state: run.state })
  }
  const runTasks = p.match(/^\/api\/orgs\/([^/]+)\/collections\/([^/]+)\/runs\/(\d+)\/tasks$/)
  if (req.method === 'GET' && runTasks) {
    const org = decodeURIComponent(runTasks[1])
    const coll = decodeURIComponent(runTasks[2])
    if (!collectionExists(org, coll)) return json(res, 404, { error: `unknown collection ${org}/${coll}` })
    const run = runs.find((r) => String(r.id) === runTasks[3])
    if (!run || run.target !== coll) return json(res, 404, { error: `run ${runTasks[3]} not found` })
    return json(res, 200, RUN_TASKS[runTasks[3]] || [])
  }
  const collResource = p.match(/^\/api\/orgs\/([^/]+)\/collections\/([^/]+)\/(hostkeys|ssh\/stats)$/)
  if (req.method === 'GET' && collResource) {
    const org = decodeURIComponent(collResource[1])
    const coll = decodeURIComponent(collResource[2])
    if (!collectionExists(org, coll)) return json(res, 404, { error: `unknown collection ${org}/${coll}` })
    if (collResource[3] === 'hostkeys') {
      return json(res, 200, hostkeys.filter((k) => k.target.split('/')[0] === coll))
    }
    const stats = {}
    for (const [scope, s] of Object.entries(SSH_STATS)) {
      if (scope.split('/')[0] === coll) stats[scope] = s
    }
    return json(res, 200, stats)
  }
  const collDecision = p.match(/^\/api\/orgs\/([^/]+)\/collections\/([^/]+)\/hostkeys\/(accept|reject)$/)
  if (req.method === 'POST' && collDecision) {
    const org = decodeURIComponent(collDecision[1])
    const coll = decodeURIComponent(collDecision[2])
    if (!collectionExists(org, coll)) return json(res, 404, { error: `unknown collection ${org}/${coll}` })
    return decideHostKey(req, res, collDecision[3], coll)
  }
  // --- API token self-service (mirrors internal/server) -------------------
  if (req.method === 'GET' && p === '/api/tokens') {
    const mine = [...tokens.values()]
      .filter((t) => t.owner === me.username && (!t.expires_at || Date.now() < new Date(t.expires_at).getTime()))
      .sort((a, b) => a.name.localeCompare(b.name))
    return json(res, 200, mine)
  }
  if (req.method === 'POST' && p === '/api/tokens') {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      let req2
      try {
        req2 = JSON.parse(body || '{}')
      } catch {
        return json(res, 400, { error: 'invalid JSON' })
      }
      const name = typeof req2.name === 'string' ? req2.name.trim() : ''
      if (!name) return json(res, 400, { error: 'name is required' })
      const dup = [...tokens.values()].some((t) => t.owner === me.username && t.name === name)
      if (dup) return json(res, 409, { error: `token "${name}" already exists` })
      const raw = TOKEN_PREFIX + randomBytes(32).toString('hex')
      const rec = {
        name, owner: me.username, prefix: raw.slice(0, TOKEN_PREFIX.length + 8),
        created_at: new Date().toISOString(), last_used_at: null,
      }
      const ttl = Number(req2.ttl_hours) || 0
      if (ttl > 0) rec.expires_at = new Date(Date.now() + ttl * HOUR).toISOString()
      tokens.set(raw, rec)
      const { owner, ...pub } = rec
      return json(res, 201, { raw, token: pub })
    })
    return
  }
  const tokenDel = p.match(/^\/api\/tokens\/([^/]+)$/)
  if (req.method === 'DELETE' && tokenDel) {
    const name = decodeURIComponent(tokenDel[1])
    for (const [raw, t] of tokens) {
      if (t.owner === me.username && t.name === name) {
        tokens.delete(raw)
        res.writeHead(204)
        return res.end()
      }
    }
    return json(res, 404, { error: `token "${name}" not found` })
  }
  const scopedRun = p.match(/^\/api\/orgs\/([^/]+)\/collections\/([^/]+)\/run$/)
  if (req.method === 'POST' && (p === '/api/run' || scopedRun)) {
    if (scopedRun) {
      const orgName = decodeURIComponent(scopedRun[1])
      const collection = decodeURIComponent(scopedRun[2])
      const org = ORGS.find((o) => o.name === orgName)
      if (!org || !org.collections.some((c) => c.name === collection)) {
        return json(res, 404, { error: `unknown collection ${collection} in org ${orgName}` })
      }
    }
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      let req2
      try {
        req2 = JSON.parse(body || '{}')
      } catch {
        return json(res, 400, { error: 'invalid JSON' })
      }
      const { playbook, check } = req2
      const target = scopedRun ? decodeURIComponent(scopedRun[2]) : req2.target
      if (!target || !playbook) {
        return json(res, 400, { error: 'target and playbook are required' })
      }
      const known = (PLAYBOOKS[target] || []).some((pb) => pb.name === playbook)
      if (!known) {
        return json(res, 404, { error: `unknown playbook ${JSON.stringify(playbook)} for target ${JSON.stringify(target)}` })
      }
      // The real API persists the run when it finishes; the mock records it
      // immediately so the UI's pending-run state resolves on its next poll.
      const startedAt = new Date()
      runs = [
        {
          id: nextRunID++, target, playbook, check: Boolean(check),
          started_at: startedAt.toISOString(), finished_at: startedAt.toISOString(),
          duration_ms: 100, exit_code: 0, success: true,
          hosts: 1, changed_hosts: 0, failed_hosts: 0,
        },
        ...runs,
      ]
      return json(res, 202, { status: 'started', target, playbook, check: Boolean(check) })
    })
    return
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
  if (url.pathname.startsWith('/api/')) {
    handleApi(req, res, url)
  } else {
    serveStatic(req, res, url)
  }
}).listen(PORT, '127.0.0.1', () => {
  console.log(`mock strangle server on http://127.0.0.1:${PORT} (serving ${dist})`)
})
