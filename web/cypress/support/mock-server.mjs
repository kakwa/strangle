// Mock strangle API for the Cypress e2e suite.
//
// Serves the production build in dist/ (with SPA fallback) plus a fixed set
// of fixture data whose JSON shapes match the real API exactly (see
// internal/server/server.go and internal/store/store.go). POST /api/run is
// stateful: it appends a new run to /api/runs so the UI's pending-run
// tracking can be exercised end to end.

import { createServer } from 'node:http'
import { readFile, stat } from 'node:fs/promises'
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
let nextRunID = 5

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

function json(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(body))
}

function handleApi(req, res, url) {
  const p = url.pathname
  if (req.method === 'GET' && p === '/api/health') {
    return json(res, 200, { status: 'ok' })
  }
  if (req.method === 'GET' && p === '/api/stats') {
    return json(res, 200, STATS)
  }
  if (req.method === 'GET' && p === '/api/runs') {
    const limit = Number(url.searchParams.get('limit') || 50)
    return json(res, 200, runs.slice(0, limit))
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
  if (req.method === 'POST' && p === '/api/run') {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      let req2
      try {
        req2 = JSON.parse(body || '{}')
      } catch {
        return json(res, 400, { error: 'invalid JSON' })
      }
      const { target, playbook } = req2
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
          id: nextRunID++, target, playbook,
          started_at: startedAt.toISOString(), finished_at: startedAt.toISOString(),
          duration_ms: 100, exit_code: 0, success: true,
          hosts: 1, changed_hosts: 0, failed_hosts: 0,
        },
        ...runs,
      ]
      return json(res, 202, { status: 'started', target, playbook })
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
