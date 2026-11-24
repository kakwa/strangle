import { useCallback, useEffect, useState } from 'react'
import {
  IconBook,
  IconClock,
  IconPlayerPlay,
  IconServer,
} from '@tabler/icons-react'
import {
  Accordion,
  Alert,
  AppShell,
  Badge,
  Burger,
  Button,
  Card,
  Center,
  Group,
  Loader,
  ScrollArea,
  SimpleGrid,
  Stack,
  Table,
  Text,
  ThemeIcon,
  Title,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { DoubleNavbar } from './DoubleNavbar'

const POLL_INTERVAL_MS = 10_000

// The API should always return arrays/objects, but older or partial responses
// may contain null. Normalize them so a rendering bug cannot take down the UI.
function asArray(value) {
  return Array.isArray(value) ? value : []
}

function asObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value
    : {}
}

function normalizeStats(value) {
  const s = asObject(value)
  return {
    hosts: {
      total: 0,
      in_inventory: 0,
      active: 0,
      disappeared: 0,
      ...asObject(s.hosts),
    },
    runs: {
      total: 0,
      succeeded: 0,
      failed: 0,
      avg_duration_ms: undefined,
      last_run_at: null,
      ...asObject(s.runs),
    },
    per_playbook: asArray(s.per_playbook),
  }
}

function normalizePlaybooks(value) {
  return Object.fromEntries(
    Object.entries(asObject(value)).map(([target, pbs]) => [
      target,
      asArray(pbs).map((pb) => ({
        name: pb?.name ?? '',
        playbook: pb?.playbook ?? '',
        inventory: pb?.inventory ?? '',
        last_status: pb?.last_status ?? '',
        last_run_at: pb?.last_run_at ?? null,
      })),
    ])
  )
}

function fmtDuration(ms) {
  if (ms === undefined || ms === null) return '—'
  if (ms < 1000) return `${ms}ms`
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  return `${Math.floor(s / 60)}m${s % 60}s`
}

function fmtTime(t) {
  if (!t) return '—'
  return new Date(t).toLocaleString()
}

function fmtAgo(t) {
  if (!t) return '—'
  const diff = Date.now() - new Date(t).getTime()
  const m = Math.floor(diff / 60000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

function Stat({ label, value, icon: Icon, children }) {
  return (
    <Card withBorder>
      <Group gap="sm" align="center">
        <ThemeIcon size={40} variant="light" color="blue">
          <Icon size={20} stroke={1.5} />
        </ThemeIcon>
        <div>
          <Text size="xs" c="dimmed" fw={700} tt="uppercase">
            {label}
          </Text>
          <Text size="xl" fw={700}>
            {value}
          </Text>
        </div>
      </Group>
      {children}
    </Card>
  )
}

const STATUS_COLORS = {
  ok: 'green',
  changed: 'blue',
  failed: 'red',
  unreachable: 'orange',
}

function StatusBadge({ status }) {
  const color = STATUS_COLORS[status] || 'gray'
  return <Badge color={color} variant="light">{status || 'unknown'}</Badge>
}

function DiffBlock({ diff, error }) {
  if (!diff && !error) return null
  return (
    <Group gap={0} align="start" wrap="nowrap">
      {diff && (
        <ScrollArea.Autosize mah={300} style={{ flex: 1, minWidth: 0 }}>
          <Text component="pre" size="xs" ff="monospace" c={error ? 'red' : 'dark'}>
            {diff}
          </Text>
        </ScrollArea.Autosize>
      )}
      {error && (
        <ScrollArea.Autosize mah={300} style={{ flex: 1, minWidth: 0 }}>
          <Text component="pre" size="xs" ff="monospace" c="red">
            {error}
          </Text>
        </ScrollArea.Autosize>
      )}
    </Group>
  )
}

function Dashboard({ stats, runs, hosts, onHost }) {
  const disappeared = hosts.filter((h) => !h.active && h.in_inventory)
  return (
    <Stack gap="md">
      <div id="dash-overview">
        <SimpleGrid cols={{ base: 2, md: 4 }} gap="md">
          <Stat label="Hosts" value={stats.hosts.total} icon={IconServer}>
            <Group gap="xs" mt="xs" wrap="wrap">
              <Badge size="sm" variant="subtle">{stats.hosts.in_inventory} in inventory</Badge>
              <Badge size="sm" color="green" variant="light">{stats.hosts.active} active</Badge>
              {stats.hosts.disappeared > 0 && (
                <Badge size="sm" color="orange" variant="light">{stats.hosts.disappeared} disappeared</Badge>
              )}
            </Group>
          </Stat>
          <Stat label="Runs" value={stats.runs.total} icon={IconPlayerPlay}>
            <Group gap="xs" mt="xs" wrap="wrap">
              <Badge size="sm" color="green" variant="light">{stats.runs.succeeded} succeeded</Badge>
              <Badge size="sm" color="red" variant="light">{stats.runs.failed} failed</Badge>
            </Group>
          </Stat>
          <Stat label="Avg duration" value={fmtDuration(stats.runs.avg_duration_ms)} icon={IconClock}>
            <Text size="xs" c="dimmed" mt="xs">last run {fmtAgo(stats.runs.last_run_at)}</Text>
          </Stat>
          <Stat label="Playbooks" value={stats.per_playbook.length} icon={IconBook}>
            <Text size="xs" c="dimmed" mt="xs">
              {stats.per_playbook.map((p) => (p.playbook || '').split('/').pop()).join(', ') || '—'}
            </Text>
          </Stat>
        </SimpleGrid>
      </div>

      {disappeared.length > 0 && (
        <Card withBorder>
          <Title order={4} mb="sm">Disappeared hosts</Title>
          <Group gap="sm" wrap="wrap">
            {disappeared.map((h) => (
              <Badge key={h.name} color="orange" variant="light" leftSection={<span>⚠</span>} onClick={() => onHost(h.name)}>
                {h.name}
              </Badge>
            ))}
          </Group>
        </Card>
      )}

      <Card withBorder id="dash-runs">
        <Title order={4} mb="sm">Recent runs</Title>
        <RunsTable runs={runs} onHost={onHost} />
      </Card>

      <Card withBorder id="dash-playbooks">
        <Title order={4} mb="sm">Per-playbook</Title>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Playbook</Table.Th>
              <Table.Th>Runs</Table.Th>
              <Table.Th>Failed</Table.Th>
              <Table.Th>Last run</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {stats.per_playbook.map((p) => (
              <Table.Tr key={`${p.target}/${p.playbook}`}>
                <Table.Td>{p.playbook}</Table.Td>
                <Table.Td>{p.total}</Table.Td>
                <Table.Td>
                  <Text component="span" c={p.failed > 0 ? 'red' : 'inherit'}>{p.failed}</Text>
                </Table.Td>
                <Table.Td>{fmtTime(p.last_run_at)}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Card>
    </Stack>
  )
}

function RunsTable({ runs, onHost }) {
  return (
    <Table>
      <Table.Thead>
        <Table.Tr>
          <Table.Th>Playbook</Table.Th>
          <Table.Th>Started</Table.Th>
          <Table.Th>Duration</Table.Th>
          <Table.Th>Result</Table.Th>
          <Table.Th>Hosts</Table.Th>
          <Table.Th>Changed</Table.Th>
          <Table.Th>Failed</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {runs.map((r) => (
          <Table.Tr key={r.id}>
            <Table.Td>{r.playbook}</Table.Td>
            <Table.Td>{fmtTime(r.started_at)}</Table.Td>
            <Table.Td>{fmtDuration(r.duration_ms)}</Table.Td>
            <Table.Td>
              <Badge color={r.success ? 'green' : 'red'} variant="light">
                {r.success ? 'ok' : `exit ${r.exit_code}`}
              </Badge>
            </Table.Td>
            <Table.Td>{r.hosts}</Table.Td>
            <Table.Td>{r.changed_hosts}</Table.Td>
            <Table.Td>
              <Text component="span" c={r.failed_hosts > 0 ? 'red' : 'inherit'}>{r.failed_hosts}</Text>
            </Table.Td>
          </Table.Tr>
        ))}
        {runs.length === 0 && (
          <Table.Tr>
            <Table.Td colSpan={7}>No runs recorded yet</Table.Td>
          </Table.Tr>
        )}
      </Table.Tbody>
    </Table>
  )
}

function HostsTable({ hosts, onHost, filter, setFilter }) {
  const visible = filter === 'all' ? hosts : hosts.filter((h) =>
    filter === 'active' ? h.active : !h.active && h.in_inventory
  )
  return (
    <Card withBorder>
      <Group justify="space-between" mb="md">
        <Title order={4}>Hosts</Title>
        <Group gap="xs">
          {['all', 'active', 'disappeared'].map((f) => (
            <Badge
              key={f}
              variant={filter === f ? 'filled' : 'subtle'}
              onClick={() => setFilter(f)}
              style={{ cursor: 'pointer' }}
            >
              {f}
            </Badge>
          ))}
        </Group>
      </Group>
      <Table>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Host</Table.Th>
            <Table.Th>IP</Table.Th>
            <Table.Th>Groups</Table.Th>
            <Table.Th>Status</Table.Th>
            <Table.Th>Last run</Table.Th>
            <Table.Th>Last change</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {visible.map((h) => (
            <Table.Tr key={h.name} onClick={() => onHost(h.name)} style={{ cursor: 'pointer' }}>
              <Table.Td>
                <Group gap="xs">
                  <Text fw="bold">{h.name}</Text>
                  {!h.in_inventory && <Badge size="xs" color="gray" variant="subtle">not in inventory</Badge>}
                  {!h.active && h.in_inventory && <Badge size="xs" color="orange" variant="light">disappeared</Badge>}
                </Group>
              </Table.Td>
              <Table.Td>{h.ip || '—'}</Table.Td>
              <Table.Td>
                <Group gap={4} wrap="wrap">
                  {(h.groups || []).map((g) => (
                    <Badge key={g} size="xs" variant="subtle">{g}</Badge>
                  ))}
                </Group>
              </Table.Td>
              <Table.Td><StatusBadge status={h.last_status} /></Table.Td>
              <Table.Td>{fmtAgo(h.last_run_at)}</Table.Td>
              <Table.Td>{h.last_change_at ? fmtAgo(h.last_change_at) : '—'}</Table.Td>
            </Table.Tr>
          ))}
          {visible.length === 0 && (
            <Table.Tr>
              <Table.Td colSpan={6}>No hosts</Table.Td>
            </Table.Tr>
          )}
        </Table.Tbody>
      </Table>
    </Card>
  )
}

function PlaybooksView({ playbooks, onRun, pendingRuns, runError }) {
  const entries = Object.entries(playbooks)
  return (
    <Card withBorder>
      <Title order={4} mb="sm">Targets & playbooks</Title>
      {runError && (
        <Alert color="red" title="Run failed" mb="md">
          {runError}
        </Alert>
      )}
      <Table>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Collection</Table.Th>
            <Table.Th>Playbook</Table.Th>
            <Table.Th>Inventory</Table.Th>
            <Table.Th>Status</Table.Th>
            <Table.Th w={110}>Action</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {entries.flatMap(([target, pbs]) =>
            pbs.map((pb, pi) => {
              const key = `${target}:${pb.name}`
              const pending = pendingRuns[key] !== undefined
              return (
                <Table.Tr key={key}>
                  {pi === 0 && (
                    <Table.Td td={pbs.length > 1 ? { rowSpan: pbs.length } : undefined}>
                      <Text fw="bold">{target}</Text>
                    </Table.Td>
                  )}
                  <Table.Td>
                    <Group gap="xs" wrap="nowrap">
                      <Text fw="semibold">{pb.name}</Text>
                      {pb.playbook && pb.playbook !== pb.name && (
                        <Text size="xs" c="dimmed" ff="monospace">{pb.playbook}</Text>
                      )}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" ff="monospace">{pb.inventory || '—'}</Text>
                  </Table.Td>
                  <Table.Td>
                    {pb.last_status ? (
                      <Group gap="xs" wrap="nowrap">
                        <StatusBadge status={pb.last_status} />
                        <Text size="xs" c="dimmed">{fmtAgo(pb.last_run_at)}</Text>
                      </Group>
                    ) : (
                      <Text size="xs" c="dimmed">never run</Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Button
                      size="xs"
                      variant="light"
                      leftSection={pending ? <Loader size={12} /> : <IconPlayerPlay size={14} />}
                      onClick={() => onRun(target, pb.name)}
                      disabled={pending}
                    >
                      {pending ? 'Running…' : 'Run'}
                    </Button>
                  </Table.Td>
                </Table.Tr>
              )
            })
          )}
          {entries.length === 0 && (
            <Table.Tr>
              <Table.Td colSpan={5}>No targets configured</Table.Td>
            </Table.Tr>
          )}
        </Table.Tbody>
      </Table>
    </Card>
  )
}

function HostDetail({ host, onBack }) {
  const [runs, setRuns] = useState([])
  const [changes, setChanges] = useState([])

  const refresh = useCallback(async () => {
    try {
      const [runsRes, changesRes] = await Promise.all([
        fetch(`/api/hosts/${encodeURIComponent(host)}/runs?limit=10`),
        fetch(`/api/hosts/${encodeURIComponent(host)}/changes?limit=100`),
      ])
      if (!runsRes.ok || !changesRes.ok) {
        throw new Error(`HTTP ${runsRes.status}/${changesRes.status}`)
      }
      setRuns(asArray(await runsRes.json()))
      setChanges(asArray(await changesRes.json()))
    } catch {
      // ignore transient errors while polling
    }
  }, [host])

  useEffect(() => {
    refresh()
    const id = setInterval(refresh, POLL_INTERVAL_MS)
    return () => clearInterval(id)
  }, [refresh])

  const info = (
    <Group gap="sm" align="center">
      <Text fw="bold">{host}</Text>
      {runs[0] && (
        <>
          <StatusBadge status={runs[0].status} />
          <Text size="xs" c="dimmed">last run {fmtAgo(runs[0].started_at)}</Text>
        </>
      )}
    </Group>
  )

  return (
    <Stack gap="md">
      <Card withBorder>
        <Group justify="space-between">
          {info}
          <Text size="sm" c="dimmed" onClick={onBack} style={{ cursor: 'pointer' }}>
            ← back to hosts
          </Text>
        </Group>
      </Card>

      <Card withBorder>
        <Title order={4} mb="sm">Last {Math.max(runs.length, 1)} runs</Title>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Run</Table.Th>
              <Table.Th>Playbook</Table.Th>
              <Table.Th>Started</Table.Th>
              <Table.Th>Status</Table.Th>
              <Table.Th>ok</Table.Th>
              <Table.Th>changed</Table.Th>
              <Table.Th>failed</Table.Th>
              <Table.Th>unreachable</Table.Th>
              <Table.Th>skipped</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {runs.map((r) => (
              <Table.Tr key={r.run_id}>
                <Table.Td>#{r.run_id}</Table.Td>
                <Table.Td>{r.playbook}</Table.Td>
                <Table.Td>{fmtTime(r.started_at)}</Table.Td>
                <Table.Td><StatusBadge status={r.status} /></Table.Td>
                <Table.Td>{r.ok}</Table.Td>
                <Table.Td>{r.changed}</Table.Td>
                <Table.Td>
                  <Text component="span" c={r.failed > 0 ? 'red' : 'inherit'}>{r.failed}</Text>
                </Table.Td>
                <Table.Td>
                  <Text component="span" c={r.unreachable > 0 ? 'orange' : 'inherit'}>{r.unreachable}</Text>
                </Table.Td>
                <Table.Td>{r.skipped}</Table.Td>
              </Table.Tr>
            ))}
            {runs.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={9}>No runs recorded for this host yet</Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>

      <Card withBorder>
        <Title order={4} mb="sm">Task changes</Title>
        <Accordion>
          {changes.map((c, i) => (
            <Accordion.Item key={`${c.task}-${i}`} value={String(i)}>
              <Accordion.Control>
                <Group justify="space-between" wrap="nowrap" style={{ minWidth: 0 }}>
                  <Text size="sm" truncate style={{ flex: 1, minWidth: 0 }}>{c.task}</Text>
                  <Group gap="xs" flexShrink={0}>
                    <Badge size="xs" color={STATUS_COLORS[c.status] || 'gray'} variant="light">{c.status}</Badge>
                    <Text size="xs" c="dimmed">{fmtAgo(c.last_run_at)}</Text>
                  </Group>
                </Group>
              </Accordion.Control>
              <Accordion.Panel>
                <Stack gap="xs">
                  <Group gap="sm">
                    <Text size="xs" c="dimmed">last run:</Text>
                    <Text size="xs">{fmtTime(c.last_run_at)}</Text>
                    {c.last_changed_at && (
                      <>
                        <Text size="xs" c="dimmed">last changed:</Text>
                        <Text size="xs">{fmtTime(c.last_changed_at)}</Text>
                      </>
                    )}
                  </Group>
                  <DiffBlock diff={c.last_diff} error={c.last_error} />
                </Stack>
              </Accordion.Panel>
            </Accordion.Item>
          ))}
          {changes.length === 0 && (
            <Text size="sm" c="dimmed">No task changes recorded for this host yet</Text>
          )}
        </Accordion>
      </Card>
    </Stack>
  )
}

export default function App() {
  const [stats, setStats] = useState(null)
  const [runs, setRuns] = useState([])
  const [hosts, setHosts] = useState([])
  const [playbooks, setPlaybooks] = useState({})
  const [error, setError] = useState(null)
  const [section, setSection] = useState('Dashboard')
  const [selectedHost, setSelectedHost] = useState(null)
  const [hostFilter, setHostFilter] = useState('all')
  // Manual runs in flight, keyed by "<target>:<playbook>" with the trigger
  // time in ms. Runs are only persisted after they finish, so the UI tracks
  // them until a matching run appears in /api/runs.
  const [pendingRuns, setPendingRuns] = useState({})
  const [runError, setRunError] = useState(null)
  const [mobileOpened, { toggle: toggleMobile, close: closeMobile }] =
    useDisclosure(false)

  const refresh = useCallback(async () => {
    try {
      const [statsRes, runsRes, hostsRes, playbooksRes] = await Promise.all([
        fetch('/api/stats'),
        fetch('/api/runs?limit=50'),
        fetch('/api/hosts'),
        fetch('/api/playbooks'),
      ])
      if (!statsRes.ok || !runsRes.ok || !hostsRes.ok || !playbooksRes.ok) {
        throw new Error(`HTTP ${statsRes.status}/${runsRes.status}/${hostsRes.status}/${playbooksRes.status}`)
      }
      const newRuns = asArray(await runsRes.json())
      setStats(normalizeStats(await statsRes.json()))
      setRuns(newRuns)
      setHosts(asArray(await hostsRes.json()))
      setPlaybooks(normalizePlaybooks(await playbooksRes.json()))
      // A manual run is done once any run of the same (target, playbook)
      // started at or after the trigger has been persisted.
      setPendingRuns((prev) => {
        let changed = false
        const next = { ...prev }
        for (const [key, triggeredAt] of Object.entries(prev)) {
          const [target, playbook] = key.split(':')
          if (newRuns.some((r) =>
            r.target === target &&
            r.playbook === playbook &&
            new Date(r.started_at).getTime() >= triggeredAt - 5000
          )) {
            delete next[key]
            changed = true
          }
        }
        return changed ? next : prev
      })
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [])

  const triggerRun = useCallback(async (target, playbook) => {
    setRunError(null)
    try {
      const res = await fetch('/api/run', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ target, playbook }),
      })
      if (res.status === 202) {
        const key = `${target}:${playbook}`
        setPendingRuns((p) => ({ ...p, [key]: Date.now() }))
      } else {
        const body = await res.json().catch(() => ({}))
        setRunError(body.error || `HTTP ${res.status}`)
      }
    } catch (err) {
      setRunError(err.message)
    }
  }, [])

  useEffect(() => {
    refresh()
    const id = setInterval(refresh, POLL_INTERVAL_MS)
    return () => clearInterval(id)
  }, [refresh])

  const openHost = (name) => {
    setSelectedHost(name)
    setSection('Hosts')
  }

  const backToHosts = () => {
    setSelectedHost(null)
  }

  const host = selectedHost ? hosts.find((h) => h.name === selectedHost) : null

  const headerTitle =
    section === 'Hosts' && selectedHost ? selectedHost : section

  return (
    <AppShell
      header={{ height: 56 }}
      navbar={{
        width: { base: 300 },
        breakpoint: 'sm',
        collapsed: { mobile: !mobileOpened },
      }}
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group gap="sm" align="center">
            <Burger
              opened={mobileOpened}
              onClick={toggleMobile}
              hiddenFrom="sm"
              size="sm"
            />
            <Title order={4}>{headerTitle}</Title>
          </Group>
          <Badge variant="subtle" color="gray" size="sm">
            polling every {POLL_INTERVAL_MS / 1000}s
          </Badge>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p={0}>
        <DoubleNavbar
          active={section}
          onSection={(s) => {
            setSection(s)
            closeMobile()
          }}
          hosts={hosts}
          selectedHost={selectedHost}
          onSelectHost={openHost}
          playbooks={playbooks}
        />
      </AppShell.Navbar>

      <AppShell.Main>
        <Stack gap="lg" p="lg">
          {error && (
            <Alert color="red" title="API unavailable">
              {error}
            </Alert>
          )}

          {stats === null && !error && (
            <Center py="xl">
              <Loader />
            </Center>
          )}

          {stats && section === 'Dashboard' && (
            <Dashboard stats={stats} runs={runs} hosts={hosts} onHost={openHost} />
          )}

          {stats && section === 'Hosts' && !selectedHost && (
            <HostsTable hosts={hosts} onHost={openHost} filter={hostFilter} setFilter={setHostFilter} />
          )}

          {stats && section === 'Hosts' && selectedHost && (
            host ? (
              <HostDetail host={selectedHost} onBack={backToHosts} />
            ) : (
              <Alert color="gray" title="Host not found">
                It may have been removed from the inventory.
              </Alert>
            )
          )}

          {stats && section === 'Playbooks' && (
            <PlaybooksView
              playbooks={playbooks}
              onRun={triggerRun}
              pendingRuns={pendingRuns}
              runError={runError}
            />
          )}
        </Stack>
      </AppShell.Main>
    </AppShell>
  )
}
