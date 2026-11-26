import { useCallback, useEffect, useState } from 'react'
import {
  IconActivity,
  IconBook,
  IconFingerprint,
  IconKey,
  IconMoonStars,
  IconSearch,
  IconServer,
  IconSun,
} from '@tabler/icons-react'
import {
  Alert,
  AppShell,
  Badge,
  Burger,
  Button,
  Card,
  Center,
  Code,
  Group,
  Input,
  Loader,
  NativeSelect,
  SimpleGrid,
  Stack,
  Table,
  Text,
  ThemeIcon,
  Title,
  UnstyledButton,
} from '@mantine/core'
import { useMantineColorScheme } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useParams,
  useSearchParams,
} from 'react-router-dom'
import { DoubleNavbar } from './DoubleNavbar'
import LoginScreen from './LoginScreen'
import { createToken, fetchMe, fetchTokens, logout, removeToken } from './auth'

const POLL_INTERVAL_MS = 10_000
const V1 = '/api/v1'

// The only API layout is the versioned, org/collection-scoped one. There are
// no flat aliases and no global hosts/runs/stats/playbooks endpoints.
function orgPath(org) {
  return `${V1}/orgs/${encodeURIComponent(org)}`
}

function collectionPath(org, collection, resource = '') {
  const base = `${orgPath(org)}/collections/${encodeURIComponent(collection)}`
  return resource ? `${base}/${resource}` : base
}

function playbookHostPath(org, collection, playbook, host, resource = '') {
  const base = `${collectionPath(org, collection)}/playbooks/${encodeURIComponent(playbook)}/hosts/${encodeURIComponent(host)}`
  return resource ? `${base}/${resource}` : base
}

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

function normalizeHosts(value) {
  const byName = new Map()
  for (const h of asArray(value)) {
    if (!h?.name) continue
    // Hosts are scoped to one collection now, but keep the last record of a
    // duplicate name instead of rendering two identical rows.
    byName.set(h.name, h)
  }
  return [...byName.values()]
}

function normalizePlaybooks(value) {
  const list = Array.isArray(value)
    ? value
    : Object.values(asObject(value)).flatMap((pbs) => asArray(pbs))
  return list.map((pb) => {
    if (typeof pb === 'string') return { name: pb, playbook: '', inventory: '' }
    return {
      name: pb?.name ?? '',
      playbook: pb?.playbook ?? '',
      inventory: pb?.inventory ?? '',
      last_status: pb?.last_status ?? '',
      last_run_at: pb?.last_run_at ?? null,
    }
  })
}

function playbookNames(detail) {
  return asArray(detail?.playbooks).map((pb) =>
    typeof pb === 'string' ? pb : pb?.name || pb?.playbook || '',
  ).filter(Boolean)
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
  approved: 'green',
  rejected: 'red',
  pending: 'orange',
}

function StatusBadge({ status }) {
  const color = STATUS_COLORS[status] || 'gray'
  return <Badge color={color} variant="light">{status || 'unknown'}</Badge>
}

const CODE_BLOCK_STYLE = {
  flex: 1,
  minWidth: 0,
  overflow: 'auto',
  maxHeight: 300,
  background: 'var(--mantine-color-gray-1)',
  border: '1px solid var(--mantine-color-default-border)',
  borderRadius: 'var(--mantine-radius-md)',
  padding: 'var(--mantine-spacing-sm)',
}

function formatFacts(facts) {
  if (!facts) return ''
  try {
    const value = typeof facts === 'string' ? JSON.parse(facts) : facts
    if (value === null || typeof value !== 'object' || Object.keys(value).length === 0) return ''
    return JSON.stringify(value, null, 2)
  } catch {
    return String(facts)
  }
}

// ScopePicker is shared by every collection-scoped screen. Both levels are
// explicit: the org decides which collection list is offered, and the
// collection is stored in the URL so a scope is bookmarkable.
function ScopePicker({ orgs, org, collection, onChange, loading }) {
  const collections = orgs.find((o) => o.name === org)?.collections || []
  return (
    <Card withBorder>
      <Group align="flex-end" gap="md" wrap="wrap">
        <NativeSelect
          label="Organization"
          data={orgs.map((o) => o.name)}
          value={org}
          onChange={(event) => onChange(event.currentTarget.value, '')}
          style={{ minWidth: 200 }}
        />
        <NativeSelect
          label="Collection"
          data={collections.map((c) => c.name)}
          value={collection}
          onChange={(event) => onChange(org, event.currentTarget.value)}
          disabled={collections.length === 0}
          style={{ minWidth: 200 }}
        />
        {loading && <Loader size="sm" aria-label="Loading collection" />}
      </Group>
    </Card>
  )
}

// The dashboard has no stats endpoint to call: every number is counted from
// the collection detail plus that collection's hosts and playbooks lists.
function Dashboard({ data }) {
  const entries = data || []
  const allHosts = entries.flatMap((e) => e.hosts || [])
  const allPlaybooks = entries.flatMap((e) => e.playbooks || [])
  const inInventory = allHosts.filter((h) => h.in_inventory).length
  const active = allHosts.filter((h) => h.active).length
  const disappeared = allHosts.filter((h) => !h.active && h.in_inventory).length
  const failed = allPlaybooks.filter((pb) => pb.last_status === 'failed').length
  const lastRuns = allPlaybooks.map((pb) => pb.last_run_at).filter(Boolean).sort()
  const namesOf = (entry) => {
    const fromDetail = playbookNames(entry.detail)
    return fromDetail.length
      ? fromDetail
      : (entry.playbooks || []).map((pb) => pb.name).filter(Boolean)
  }
  const names = entries.flatMap(namesOf)

  return (
    <Stack gap="md">
      <div id="dash-overview">
        <SimpleGrid cols={{ base: 2, md: 4 }} gap="md">
          <Stat label="Hosts" value={allHosts.length} icon={IconServer}>
            <Group gap="xs" mt="xs" wrap="wrap">
              <Badge size="sm" variant="subtle">{inInventory} in inventory</Badge>
              <Badge size="sm" color="green" variant="light">{active} active</Badge>
              {disappeared > 0 && (
                <Badge size="sm" color="orange" variant="light">{disappeared} disappeared</Badge>
              )}
            </Group>
          </Stat>
          <Stat label="Playbooks" value={allPlaybooks.length} icon={IconBook}>
            <Text size="xs" c="dimmed" mt="xs">{names.join(', ') || '—'}</Text>
          </Stat>
          <Stat label="Collections" value={entries.length} icon={IconServer}>
            <Text size="xs" c="dimmed" mt="xs">
              {[...new Set(entries.map((e) => e.org))].join(', ') || '—'}
            </Text>
          </Stat>
          <Stat label="Failed playbooks" value={failed} icon={IconActivity}>
            <Text size="xs" c="dimmed" mt="xs">
              last run {fmtAgo(lastRuns[lastRuns.length - 1])}
            </Text>
          </Stat>
        </SimpleGrid>
      </div>

      <Card withBorder id="dash-collections">
        <Title order={4} mb="sm">Collections</Title>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Organization</Table.Th>
              <Table.Th>Collection</Table.Th>
              <Table.Th>Hosts</Table.Th>
              <Table.Th>Active</Table.Th>
              <Table.Th>Playbooks</Table.Th>
              <Table.Th>Configured playbooks</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {entries.map((e) => (
              <Table.Tr key={`${e.org}/${e.collection}`}>
                <Table.Td>{e.org}</Table.Td>
                <Table.Td>{e.collection}</Table.Td>
                <Table.Td>{(e.hosts || []).length}</Table.Td>
                <Table.Td>{(e.hosts || []).filter((h) => h.active).length}</Table.Td>
                <Table.Td>{(e.playbooks || []).length}</Table.Td>
                <Table.Td>{namesOf(e).join(', ') || '—'}</Table.Td>
              </Table.Tr>
            ))}
            {entries.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={6}>No collections configured</Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
    </Stack>
  )
}

function HostsTable({ hosts, onHost, filter, setFilter, loading }) {
  const [query, setQuery] = useState('')
  const needle = query.trim().toLowerCase()
  const visible = hosts.filter((h) => {
    const matchesFilter = filter === 'all' ||
      (filter === 'active' ? h.active : !h.active && h.in_inventory)
    return matchesFilter && (!needle || h.name.toLowerCase().includes(needle))
  })
  return (
    <Card withBorder>
      <Group justify="space-between" mb="md">
        <Title order={4}>Hosts</Title>
        <Group gap="xs">
          <Input
            aria-label="Search hosts by name"
            placeholder="Search hosts by name"
            leftSection={<IconSearch size={14} />}
            value={query}
            onChange={(event) => setQuery(event.currentTarget.value)}
            style={{ width: 220 }}
          />
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
              <Table.Td colSpan={6}>
                {loading ? 'Loading hosts…' : 'No hosts in this collection'}
              </Table.Td>
            </Table.Tr>
          )}
        </Table.Tbody>
      </Table>
    </Card>
  )
}

function PlaybooksView({ playbooks, loading }) {
  return (
    <Card withBorder>
      <Title order={4} mb="sm">Playbooks</Title>
      <Table>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Playbook</Table.Th>
            <Table.Th>Path</Table.Th>
            <Table.Th>Inventory</Table.Th>
            <Table.Th>Status</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {playbooks.map((pb) => (
            <Table.Tr key={pb.name}>
              <Table.Td><Text fw="semibold">{pb.name}</Text></Table.Td>
              <Table.Td>
                <Text size="sm" ff="monospace">{pb.playbook || '—'}</Text>
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
            </Table.Tr>
          ))}
          {playbooks.length === 0 && (
            <Table.Tr>
              <Table.Td colSpan={4}>
                {loading ? 'Loading playbooks…' : 'No playbooks configured for this collection'}
              </Table.Td>
            </Table.Tr>
          )}
        </Table.Tbody>
      </Table>
    </Card>
  )
}

// HostDetail reads runs from the playbook-scoped host URL. The playbook is an
// explicit selector because runs are no longer available through a global
// host endpoint. Host task changes were removed from the API entirely.
function HostDetail({ host, record, org, collection, playbooks, onBack }) {
  const [playbook, setPlaybook] = useState('')
  const [runs, setRuns] = useState([])
  const [error, setError] = useState(null)
  const options = playbooks.map((pb) => pb.name).filter(Boolean)

  useEffect(() => {
    setPlaybook('')
  }, [org, collection, host])

  useEffect(() => {
    if (!playbook && options.length > 0) setPlaybook(options[0])
  }, [options, playbook])

  useEffect(() => {
    setRuns([])
    setError(null)
    if (!org || !collection || !playbook) return undefined
    let active = true
    const refresh = async () => {
      try {
        const res = await fetch(
          playbookHostPath(org, collection, playbook, host, `runs?limit=10`),
        )
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        const data = asArray(await res.json())
        if (active) {
          setRuns(data)
          setError(null)
        }
      } catch (err) {
        if (active) setError(err.message)
      }
    }
    refresh()
    const id = setInterval(refresh, POLL_INTERVAL_MS)
    return () => {
      active = false
      clearInterval(id)
    }
  }, [org, collection, playbook, host])

  const facts = formatFacts(record?.facts)

  return (
    <Stack gap="md">
      <Card withBorder>
        <Group justify="space-between">
          <Group gap="sm" align="center">
            <Text fw="bold">{host}</Text>
            <Text size="xs" c="dimmed">{org}/{collection}</Text>
            {runs[0] && (
              <>
                <StatusBadge status={runs[0].status} />
                <Text size="xs" c="dimmed">last run {fmtAgo(runs[0].started_at)}</Text>
              </>
            )}
          </Group>
          <Text size="sm" c="dimmed" onClick={onBack} style={{ cursor: 'pointer' }}>
            ← back to hosts
          </Text>
        </Group>
      </Card>

      {facts && (
        <Card withBorder>
          <Title order={4} mb="sm">Ansible facts</Title>
          <div style={CODE_BLOCK_STYLE}>
            <Text component="pre" size="xs" ff="monospace" style={{ margin: 0 }}>
              {facts}
            </Text>
          </div>
        </Card>
      )}

      <Card withBorder>
        <Group justify="space-between" mb="sm" wrap="wrap">
          <Title order={4}>Playbook runs</Title>
          <NativeSelect
            aria-label="Playbook"
            label="Playbook"
            data={options}
            value={playbook}
            onChange={(event) => setPlaybook(event.currentTarget.value)}
            style={{ minWidth: 220 }}
          />
        </Group>
        {error && <Alert color="red" mb="sm" title="Could not load runs">{error}</Alert>}
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Run</Table.Th>
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
                <Table.Td colSpan={8}>
                  {options.length === 0
                    ? 'No playbooks configured for this collection'
                    : `No runs recorded for this host in ${playbook || 'this playbook'} yet`}
                </Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
    </Stack>
  )
}

// SSHView targets one playbook/host inside one collection. Host keys are read
// from the playbook/host URL and decisions are PATCH JSON-Patch documents;
// the old collection-key list and accept/reject POSTs no longer exist.
function SSHView({ org, collection, hosts, playbooks }) {
  const [playbook, setPlaybook] = useState('')
  const [host, setHost] = useState('')
  const [keys, setKeys] = useState([])
  const [error, setError] = useState(null)
  const [actionError, setActionError] = useState(null)
  const [busy, setBusy] = useState(null)

  const playbookOptions = playbooks.map((pb) => pb.name).filter(Boolean)
  const hostOptions = [...new Set(hosts.flatMap((h) => [h.ip, h.name]).filter(Boolean))]

  useEffect(() => {
    if (!playbook && playbookOptions.length > 0) setPlaybook(playbookOptions[0])
    if (!host && hostOptions.length > 0) setHost(hostOptions[0])
  }, [playbookOptions, host, hostOptions, playbook])

  useEffect(() => {
    setKeys([])
    setError(null)
    if (!org || !collection || !playbook || !host) return undefined
    let active = true
    const refresh = async () => {
      try {
        const res = await fetch(playbookHostPath(org, collection, playbook, host, 'hostkeys'))
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        const data = asArray(await res.json())
        if (active) {
          setKeys(data)
          setError(null)
        }
      } catch (err) {
        if (active) setError(err.message)
      }
    }
    refresh()
    const id = setInterval(refresh, POLL_INTERVAL_MS)
    return () => {
      active = false
      clearInterval(id)
    }
  }, [org, collection, playbook, host])

  const decide = async (status, key) => {
    setActionError(null)
    setBusy(`${status}|${key.host}`)
    try {
      const patch = [{ op: 'replace', path: '/status', value: status }]
      if (status === 'approved' && key.fingerprint) {
        patch.push({ op: 'replace', path: '/fingerprint', value: key.fingerprint })
      }
      const res = await fetch(playbookHostPath(org, collection, playbook, host, 'hostkeys'), {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(patch),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error(data.error || `HTTP ${res.status}`)
      }
      const updated = asObject(await res.json())
      setKeys((prev) => prev.map((k) =>
        k.host === updated.host ? { ...k, status: updated.status } : k,
      ))
    } catch (err) {
      setActionError(err.message)
    } finally {
      setBusy(null)
    }
  }

  return (
    <Stack gap="md">
      <Card withBorder>
        <Group justify="space-between" wrap="wrap">
          <Group gap="sm">
            <ThemeIcon variant="light" color="orange" size={32}>
              <IconFingerprint size={18} stroke={1.5} />
            </ThemeIcon>
            <Title order={4}>Host keys</Title>
            {keys.length > 0 && <Badge color="orange" variant="light">{keys.length}</Badge>}
          </Group>
          <Text size="xs" c="dimmed">
            {org}/{collection}
          </Text>
        </Group>
        <Group align="flex-end" gap="md" mt="sm" wrap="wrap">
          <NativeSelect
            aria-label="SSH playbook"
            label="Playbook"
            data={playbookOptions}
            value={playbook}
            onChange={(event) => {
              setPlaybook(event.currentTarget.value)
              setHost('')
            }}
            style={{ minWidth: 220 }}
          />
          <NativeSelect
            aria-label="SSH host"
            label="Host"
            data={hostOptions}
            value={host}
            onChange={(event) => setHost(event.currentTarget.value)}
            style={{ minWidth: 220 }}
          />
        </Group>
        <Text size="xs" c="dimmed" mt="xs">
          Unknown or changed SSH keys are held here until you approve or reject
          them for this playbook and host.
        </Text>
        {error && <Alert color="red" mt="sm" title="API unavailable">{error}</Alert>}
        {actionError && <Alert color="red" mt="sm" title="Could not update the host key">{actionError}</Alert>}
        <Table mt="sm">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Playbook</Table.Th>
              <Table.Th>Host</Table.Th>
              <Table.Th>Fingerprint / key</Table.Th>
              <Table.Th>First seen</Table.Th>
              <Table.Th>Status</Table.Th>
              <Table.Th>Actions</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {keys.map((key) => (
              <Table.Tr key={`${key.playbook || playbook}|${key.host}`}>
                <Table.Td><Code>{key.playbook || playbook}</Code></Table.Td>
                <Table.Td>{key.host}</Table.Td>
                <Table.Td>
                  <Stack gap={2}>
                    <Text size="xs" ff="monospace">{key.fingerprint}</Text>
                    {key.public_key && (
                      <Text size="xs" c="dimmed" ff="monospace" style={{ wordBreak: 'break-all' }}>
                        {key.public_key.trim()}
                      </Text>
                    )}
                  </Stack>
                </Table.Td>
                <Table.Td>{fmtTime(key.first_seen)}</Table.Td>
                <Table.Td><StatusBadge status={key.status || 'pending'} /></Table.Td>
                <Table.Td>
                  <Group gap="xs" wrap="nowrap">
                    <Button
                      size="xs"
                      color="green"
                      variant="light"
                      loading={busy === `approved|${key.host}`}
                      onClick={() => decide('approved', key)}
                    >
                      Approve
                    </Button>
                    <Button
                      size="xs"
                      color="red"
                      variant="light"
                      loading={busy === `rejected|${key.host}`}
                      onClick={() => decide('rejected', key)}
                    >
                      Reject
                    </Button>
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
            {keys.length === 0 && !error && (
              <Table.Tr>
                <Table.Td colSpan={6}>
                  {playbook && host
                    ? 'No host keys recorded for this playbook and host.'
                    : 'Select a playbook and a host to inspect their keys.'}
                </Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
    </Stack>
  )
}

// TokensView is collection-scoped: there is no global token list anymore.
function TokensView({ org, collection }) {
  const [tokens, setTokens] = useState(null)
  const [loadError, setLoadError] = useState(null)
  const [name, setName] = useState('')
  const [ttlHours, setTtlHours] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [created, setCreated] = useState(null)

  const refresh = useCallback(async () => {
    if (!org || !collection) return
    try {
      setTokens(asArray(await fetchTokens(org, collection)))
      setLoadError(null)
    } catch (err) {
      setLoadError(err.message)
    }
  }, [org, collection])

  useEffect(() => {
    setTokens(null)
    setCreated(null)
    refresh()
  }, [refresh])

  const create = async (e) => {
    e.preventDefault()
    const trimmed = name.trim()
    if (!trimmed || busy) return
    setBusy(true)
    setError(null)
    try {
      setCreated(await createToken(org, collection, trimmed, Number(ttlHours) || 0))
      setName('')
      setTtlHours('')
      await refresh()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (t) => {
    setError(null)
    try {
      await removeToken(org, collection, t.name)
      if (created && created.token && created.token.name === t.name) setCreated(null)
      await refresh()
    } catch (err) {
      setError(err.message)
    }
  }

  if (!org || !collection) {
    return (
      <Alert color="gray" title="No collection selected">
        Select an organization and a collection to manage its API tokens.
      </Alert>
    )
  }

  if (tokens === null && loadError) {
    return (
      <Alert color="gray" title="Token management is unavailable">
        {loadError}
      </Alert>
    )
  }

  return (
    <Stack gap="md">
      <Card withBorder>
        <Group justify="space-between" mb="xs">
          <Title order={4}>Your API tokens</Title>
          <Text size="xs" c="dimmed">{org}/{collection}</Text>
        </Group>
        <Text size="sm" c="dimmed" mb="md">
          A token authenticates <Code>/api/v1</Code> requests as your account
          for this collection. Send it as <Code>Authorization: Bearer st_…</Code>
          {' '}or as an <Code>X-Strangle-Token</Code> header.
        </Text>
        <form onSubmit={create}>
          <Group align="flex-end" gap="sm">
            <Input
              label="Name"
              placeholder="e.g. ci, laptop"
              value={name}
              onChange={(e) => setName(e.target.value)}
              style={{ maxWidth: 260 }}
            />
            <Input
              label="Lifetime (hours, optional)"
              type="number"
              min="0"
              placeholder="no expiry"
              value={ttlHours}
              onChange={(e) => setTtlHours(e.target.value)}
              style={{ maxWidth: 200 }}
            />
            <Button type="submit" leftSection={<IconKey size={14} />} disabled={busy || !name.trim()}>
              {busy ? 'Creating…' : 'Create token'}
            </Button>
          </Group>
        </form>
      </Card>

      {created && (
        <Alert color="teal" title={`Token ${created.token?.name || ''} created`} icon={<IconKey size={16} />}>
          <Group align="flex-start" gap="sm">
            <Text size="sm">
              Copy the token now — it is shown exactly once and can never be
              recovered:
            </Text>
            <Code copyable>{created.raw}</Code>
          </Group>
        </Alert>
      )}

      {error && <Alert color="red" title="Token request failed">{error}</Alert>}

      <Card withBorder>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Name</Table.Th>
              <Table.Th>Prefix</Table.Th>
              <Table.Th>Created</Table.Th>
              <Table.Th>Expires</Table.Th>
              <Table.Th>Last used</Table.Th>
              <Table.Th></Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(tokens || []).map((t) => (
              <Table.Tr key={t.name}>
                <Table.Td>{t.name}</Table.Td>
                <Table.Td>
                  <Text size="sm" ff="monospace">{t.prefix}…</Text>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" c="dimmed">{fmtTime(t.created_at)}</Text>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" c="dimmed">{t.expires_at ? fmtTime(t.expires_at) : 'never'}</Text>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" c="dimmed">
                    {t.last_used_at ? `last used ${fmtAgo(t.last_used_at)}` : 'never used'}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <Button size="xs" variant="light" color="red" onClick={() => remove(t)}>
                    Revoke
                  </Button>
                </Table.Td>
              </Table.Tr>
            ))}
            {(tokens || []).length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={6}>
                  <Text size="sm" c="dimmed">No tokens yet — create one above.</Text>
                </Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
    </Stack>
  )
}

function ColorSchemeToggle() {
  const { colorScheme, toggleColorScheme } = useMantineColorScheme()
  return (
    <UnstyledButton
      onClick={toggleColorScheme}
      title={colorScheme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
      aria-label="Toggle dark mode"
    >
      {colorScheme === 'dark' ? (
        <IconSun size={18} stroke={1.5} />
      ) : (
        <IconMoonStars size={18} stroke={1.5} />
      )}
    </UnstyledButton>
  )
}

// Maps a URL path to the section and (optionally) the selected host. Org and
// collection are query parameters so every scoped screen is bookmarkable.
function sectionFromPath(pathname) {
  const safeDecode = (s) => {
    try {
      return decodeURIComponent(s)
    } catch {
      return s
    }
  }
  const hostMatch = pathname.match(/^\/hosts\/(.+)$/)
  if (hostMatch) {
    return { section: 'Hosts', selectedHost: safeDecode(hostMatch[1]) }
  }
  if (pathname === '/hosts') return { section: 'Hosts', selectedHost: null }
  if (pathname === '/playbooks') return { section: 'Playbooks', selectedHost: null }
  if (pathname === '/tokens') return { section: 'Tokens', selectedHost: null }
  if (pathname === '/ssh') return { section: 'SSH', selectedHost: null }
  return { section: 'Dashboard', selectedHost: null }
}

function HostDetailRoute({ hosts, org, collection, playbooks, loading, onBack }) {
  const { host } = useParams()
  if (loading) {
    return <Center py="xl"><Loader /></Center>
  }
  const record = hosts.find((h) => h.name === host)
  if (!record) {
    return (
      <Alert color="gray" title="Host not found">
        It may have been removed from the inventory.
      </Alert>
    )
  }
  return (
    <HostDetail
      host={host}
      record={record}
      org={org}
      collection={collection}
      playbooks={playbooks}
      onBack={onBack}
    />
  )
}

export default function App() {
  // undefined: checking; null: auth on, not logged in; object: who we are.
  const [me, setMe] = useState(undefined)
  const [loadError, setLoadError] = useState(null)

  const loadMe = useCallback(async () => {
    try {
      setMe(await fetchMe())
    } catch (err) {
      setLoadError(err.message)
    }
  }, [])

  useEffect(() => {
    loadMe()
  }, [loadMe])

  if (loadError) {
    return (
      <Center h="100vh">
        <Alert color="red" title="Could not reach the server">
          {loadError}
        </Alert>
      </Center>
    )
  }
  if (me === undefined) {
    return (
      <Center h="100vh">
        <Loader />
      </Center>
    )
  }
  if (me === null) {
    return <LoginScreen onSuccess={setMe} />
  }
  return (
    <AppInner
      user={me.username}
      onLogout={async () => {
        await logout()
        setMe(null)
      }}
    />
  )
}

function AppInner({ user, onLogout }) {
  const [orgs, setOrgs] = useState(null)
  const [collectionData, setCollectionData] = useState({})
  const [error, setError] = useState(null)
  const [hostFilter, setHostFilter] = useState('all')
  const [tick, setTick] = useState(0)
  const [mobileOpened, { toggle: toggleMobile, close: closeMobile }] =
    useDisclosure(false)

  const location = useLocation()
  const navigate = useNavigate()
  const [params, setParams] = useSearchParams()
  const { section, selectedHost } = sectionFromPath(location.pathname)
  const selectedOrg = params.get('org') || ''
  const selectedCollection = params.get('collection') || ''
  const scopeKey = `${selectedOrg}/${selectedCollection}`
  const current = collectionData[scopeKey]
  const scopeLoaded = Boolean(current)
  const hosts = current?.hosts || []
  const playbooks = current?.playbooks || []

  // /api/v1/orgs is the single discovery read; it already carries each org's
  // collections. No stats endpoint and no extra per-org collection listing is
  // needed to discover the scope.
  const refreshOrgs = useCallback(async () => {
    try {
      const res = await fetch(`${V1}/orgs`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const raw = asArray(await res.json())
      setOrgs(raw.map((org) => ({
        ...org,
        collections: asArray(org?.collections),
      })))
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => {
    refreshOrgs()
  }, [refreshOrgs, tick])

  useEffect(() => {
    const id = setInterval(() => setTick((n) => n + 1), POLL_INTERVAL_MS)
    return () => clearInterval(id)
  }, [])

  // Fill the URL with a valid scope once orgs are known, and repair stale
  // deep links (an org/collection that no longer exists).
  useEffect(() => {
    if (!orgs || orgs.length === 0) return
    const pick = (current, list) => list.some((x) => x.name === current)
      ? current
      : list[0]?.name || ''
    const org = pick(selectedOrg, orgs)
    const collections = orgs.find((o) => o.name === org)?.collections || []
    const collection = pick(selectedCollection, collections)
    if (org === selectedOrg && collection === selectedCollection) return
    const next = new URLSearchParams(params)
    if (org) next.set('org', org)
    else next.delete('org')
    if (collection) next.set('collection', collection)
    else next.delete('collection')
    setParams(next, { replace: true })
  }, [orgs, params, selectedOrg, selectedCollection, setParams])

  // Per-collection data: detail, hosts and playbooks. These are also the
  // source of every dashboard count; there is no stats endpoint at all.
  useEffect(() => {
    if (!orgs) return undefined
    let active = true
    ;(async () => {
      try {
        const pairs = orgs.flatMap((org) =>
          asArray(org.collections)
            .filter((coll) => coll?.name)
            .map((coll) => [org.name, coll.name]),
        )
        const entries = await Promise.all(pairs.map(async ([org, collection]) => {
          const [detailRes, hostsRes, playbooksRes] = await Promise.all([
            fetch(collectionPath(org, collection)),
            fetch(collectionPath(org, collection, 'hosts')),
            fetch(collectionPath(org, collection, 'playbooks')),
          ])
          if (!detailRes.ok || !hostsRes.ok || !playbooksRes.ok) {
            throw new Error(`HTTP ${detailRes.status}/${hostsRes.status}/${playbooksRes.status}`)
          }
          return [`${org}/${collection}`, {
            org,
            collection,
            detail: asObject(await detailRes.json()),
            hosts: normalizeHosts(await hostsRes.json()),
            playbooks: normalizePlaybooks(await playbooksRes.json()),
          }]
        }))
        if (active) {
          setCollectionData(Object.fromEntries(entries))
          setError(null)
        }
      } catch (err) {
        if (active) setError(err.message)
      }
    })()
    return () => { active = false }
  }, [orgs, tick])

  const setScope = (org, collection) => {
    const next = new URLSearchParams(params)
    if (org) next.set('org', org)
    else next.delete('org')
    if (collection) next.set('collection', collection)
    else next.delete('collection')
    next.delete('playbook')
    next.delete('host')
    setParams(next)
  }

  // Navigation keeps the current scope so switching sections does not lose
  // the selected collection.
  const withScope = (path) => `${path}${location.search || ''}`
  const openHost = (name) => {
    closeMobile()
    navigate(withScope(`/hosts/${encodeURIComponent(name)}`))
  }
  const goSection = (label) => {
    closeMobile()
    if (label === 'Hosts') navigate(withScope('/hosts'))
    else if (label === 'Playbooks') navigate(withScope('/playbooks'))
    else if (label === 'Tokens') navigate(withScope('/tokens'))
    else if (label === 'SSH') navigate(withScope('/ssh'))
    else navigate(withScope('/'))
  }

  if (orgs === null && !error) {
    return (
      <Center h="100vh">
        <Loader />
      </Center>
    )
  }

  const headerTitle = section === 'Hosts' && selectedHost
    ? selectedHost
    : section

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
          <Group gap="sm">
            {user && (
              <>
                <Text size="sm" c="dimmed">{user}</Text>
                <Button size="xs" variant="subtle" onClick={onLogout}>Log out</Button>
              </>
            )}
            <ColorSchemeToggle />
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p={0}>
        <DoubleNavbar
          active={section}
          onSection={goSection}
          hosts={hosts}
          selectedHost={selectedHost}
          playbooks={playbooks}
        />
      </AppShell.Navbar>

      <AppShell.Main>
        <Stack gap="lg" p="lg">
          {error && (
            <Alert color="red" title="API unavailable">{error}</Alert>
          )}

          <Routes>
            <Route path="/" element={<Dashboard data={Object.values(collectionData)} />} />
            <Route
              path="/hosts"
              element={
                <Stack gap="md">
                  <ScopePicker
                    orgs={orgs || []}
                    org={selectedOrg}
                    collection={selectedCollection}
                    onChange={setScope}
                    loading={!scopeLoaded}
                  />
                  <HostsTable
                    hosts={hosts}
                    onHost={openHost}
                    filter={hostFilter}
                    setFilter={setHostFilter}
                    loading={!scopeLoaded}
                  />
                </Stack>
              }
            />
            <Route
              path="/hosts/:host"
              element={
                <HostDetailRoute
                  hosts={hosts}
                  org={selectedOrg}
                  collection={selectedCollection}
                  playbooks={playbooks}
                  loading={!scopeLoaded}
                  onBack={() => navigate(withScope('/hosts'))}
                />
              }
            />
            <Route
              path="/playbooks"
              element={
                <Stack gap="md">
                  <ScopePicker
                    orgs={orgs || []}
                    org={selectedOrg}
                    collection={selectedCollection}
                    onChange={setScope}
                    loading={!scopeLoaded}
                  />
                  <PlaybooksView playbooks={playbooks} loading={!scopeLoaded} />
                </Stack>
              }
            />
            <Route
              path="/ssh"
              element={
                <Stack gap="md">
                  <ScopePicker
                    orgs={orgs || []}
                    org={selectedOrg}
                    collection={selectedCollection}
                    onChange={setScope}
                    loading={!scopeLoaded}
                  />
                  <SSHView
                    key={scopeKey}
                    org={selectedOrg}
                    collection={selectedCollection}
                    hosts={hosts}
                    playbooks={playbooks}
                  />
                </Stack>
              }
            />
            <Route
              path="/tokens"
              element={
                <Stack gap="md">
                  <ScopePicker
                    orgs={orgs || []}
                    org={selectedOrg}
                    collection={selectedCollection}
                    onChange={setScope}
                    loading={!scopeLoaded}
                  />
                  <TokensView org={selectedOrg} collection={selectedCollection} />
                </Stack>
              }
            />

            <Route path="*" element={<Navigate to={withScope('/')} replace />} />
          </Routes>
        </Stack>
      </AppShell.Main>
    </AppShell>
  )
}
