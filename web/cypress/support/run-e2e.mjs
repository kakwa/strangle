// Runs the Cypress e2e suite against the local mock strangle server.
//
// Cypress 16 removed the e2e `webServer` config option, so the mock server is
// managed here: it is spawned before `cypress run` starts, the runner waits
// for it to answer /api/v1/health, and it is killed when the run finishes.
import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const PORT = Number(process.env.MOCK_PORT || 18090)
const BASE = `http://127.0.0.1:${PORT}`

const mock = spawn(
  process.execPath,
  [path.join(here, 'mock-server.mjs')],
  { env: { ...process.env, MOCK_PORT: String(PORT) }, stdio: 'inherit' },
)

function waitForHealth(timeoutMs) {
  const deadline = Date.now() + timeoutMs
  return new Promise((resolve, reject) => {
    const tick = async () => {
      try {
        const res = await fetch(`${BASE}/api/v1/health`)
        if (res.ok) return resolve()
      } catch {
        // not up yet
      }
      if (Date.now() > deadline) {
        return reject(new Error(`mock server did not become ready at ${BASE}`))
      }
      setTimeout(tick, 100)
    }
    tick()
  })
}

// Optional browser selection, e.g. CYPRESS_BROWSER=/usr/bin/chromium.
// Unset means Cypress picks its default (Chrome in CI, Electron locally).
const args = ['cypress', 'run']
if (process.env.CYPRESS_BROWSER) {
  args.push('--browser', process.env.CYPRESS_BROWSER)
}

let code = 1
try {
  await waitForHealth(30_000)
  const cypress = spawn('npx', args, {
    cwd: path.join(here, '..', '..'),
    stdio: 'inherit',
  })
  code = await new Promise((resolve) => cypress.on('close', resolve))
} finally {
  mock.kill()
}
process.exit(code)
