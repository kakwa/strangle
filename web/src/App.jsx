import { useCallback, useEffect, useState } from 'react'
import {
  Accordion,
  Badge,
  Card,
  Container,
  Group,
  ScrollArea,
  SimpleGrid,
  Stack,
  Table,
  Tabs,
  Text,
  Title,
} from '@mantine/core'

const POLL_INTERVAL_MS = 10_000
const PAGE_SIZE = 50

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

function Stat({ label, value, children }) {
  return (
    <Card withBorder>
      <Text size="xs" c="dimmed" fw={700} tt="uppercase">{label}</Text>
      <Text size="xl" fw={700}>{value}</Text>
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
      <SimpleGrid cols={{ base: 2, md: 4 }} gap="md">
        <Stat label="Hosts" value={stats.hosts.total}>
          <Group gap="xs" mt="xs" wrap="wrap">
            <Badge size="sm" variant="subtle">{stats.hosts.in_inventory} in inventory</Badge>
            <Badge size="sm" color="green" variant="light">{stats.hosts.active} active</Badge>
            {stats.hosts.disappeared > 0 && (
              <Badge size="sm" color="orange" variant="light">{stats.hosts.disappeared} disappeared</Badge>
            )}
          </Group>
        </Stat>
        <Stat label="Runs" value={stats.runs.total}>
          <Group gap="xs" mt="xs" wrap="wrap">
            <Badge size="sm" color="green" variant="light">{stats.runs.succeeded} succeeded</Badge>
            <Badge size="sm" color="red" variant="light">{stats.runs.failed} failed</Badge>
          </Group>
        </Stat>
        <Stat label="Avg duration" value={fmtDuration(stats.runs.avg_duration_ms)}>
          <Text size="xs" c="dimmed" mt="xs">last run {fmtAgo(stats.runs.last_run_at)}</Text>
        </Stat>
        <Stat label="Playbooks" value={stats.per_playbook.length}>
          <Text size="xs" c="dimmed" mt="xs">
            {stats.per_playbook.map((p) => p.playbook.split('/').pop()).join(', ') || '—'}
          </Text>
        </Stat>
      </SimpleGrid>

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

      <Card withBorder>
        <Title order={4} mb="sm">Recent runs</Title>
        <RunsTable runs={runs} onHost={onHost} />
      </Card>

      <Card withBorder>
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
              <Table.Tr key={p.playbook}>
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
      setRuns(await runsRes.json())
      setChanges(await changesRes.json())
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
  const [error, setError] = useState(null)
  const [tab, setTab] = useState('dashboard')
  const [selectedHost, setSelectedHost] = useState(null)
  const [hostFilter, setHostFilter] = useState('all')

  const refresh = useCallback(async () => {
    try {
      const [statsRes, runsRes, hostsRes] = await Promise.all([
        fetch('/api/stats'),
        fetch('/api/runs?limit=50'),
        fetch('/api/hosts'),
      ])
      if (!statsRes.ok || !runsRes.ok || !hostsRes.ok) {
        throw new Error(`HTTP ${statsRes.status}/${runsRes.status}/${hostsRes.status}`)
      }
      setStats(await statsRes.json())
      setRuns(await runsRes.json())
      setHosts(await hostsRes.json())
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => {
    refresh()
    const id = setInterval(refresh, POLL_INTERVAL_MS)
    return () => clearInterval(id)
  }, [refresh])

  const openHost = (name) => {
    setSelectedHost(name)
    setTab('host')
  }

  const host = selectedHost ? hosts.find((h) => h.name === selectedHost) : null

  return (
    <Container size="lg" py="md">
      <Stack gap="lg">
        <Group justify="space-between" align="center">
          <Group align="baseline" gap="sm">
            <Title order={1}>strangle</Title>
            <Text c="dimmed" size="sm">
              Ansible fleet monitor
            </Text>
          </Group>
          <Badge variant="subtle" color="gray">
            polling every {POLL_INTERVAL_MS / 1000}s
          </Badge>
        </Group>

        {error && (
          <Card withBorder>
            <Text c="red" size="sm">API unavailable: {error}</Text>
          </Card>
        )}

        {stats && (
          <Tabs value={tab} onChange={(v) => (v && setTab(v))} color="blue">
            <Tabs.List>
              <Tabs.Tab value="dashboard">Dashboard</Tabs.Tab>
              <Tabs.Tab value="hosts">Hosts</Tabs.Tab>
              {selectedHost && (
                <Tabs.Tab value="host" leftSection={<span>›</span>}>
                  {selectedHost}
                </Tabs.Tab>
              )}
            </Tabs.List>
            <Tabs.Panel value="dashboard" pt="md">
              <Dashboard stats={stats} runs={runs} hosts={hosts} onHost={openHost} />
            </Tabs.Panel>
            <Tabs.Panel value="hosts" pt="md">
              <HostsTable hosts={hosts} onHost={openHost} filter={hostFilter} setFilter={setHostFilter} />
            </Tabs.Panel>
            <Tabs.Panel value="host" pt="md">
              {host ? (
                <HostDetail host={selectedHost} onBack={() => { setSelectedHost(null); setTab('hosts') }} />
              ) : (
                <Text c="dimmed">Host not found (it may have been removed from the inventory)</Text>
              )}
            </Tabs.Panel>
          </Tabs>
        )}
      </Stack>
    </Container>
  )
}
