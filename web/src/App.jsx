import { useCallback, useEffect, useState } from 'react'
import {
  Anchor,
  Alert,
  Badge,
  Card,
  Container,
  Group,
  Stack,
  Table,
  Text,
  Title,
} from '@mantine/core'

const POLL_INTERVAL_MS = 10_000

export default function App() {
  const [jobs, setJobs] = useState([])
  const [reports, setReports] = useState([])
  const [error, setError] = useState(null)

  const refresh = useCallback(async () => {
    try {
      const [jobsRes, reportsRes] = await Promise.all([
        fetch('/api/jobs'),
        fetch('/api/reports'),
      ])
      if (!jobsRes.ok || !reportsRes.ok) {
        throw new Error(`HTTP ${jobsRes.status}/${reportsRes.status}`)
      }
      setJobs(await jobsRes.json())
      setReports(await reportsRes.json())
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

  return (
    <Container size="lg" py="md">
      <Stack gap="lg">
        <Group justify="space-between" align="center">
          <Group align="baseline" gap="sm">
            <Title order={1}>strangle</Title>
            <Text c="dimmed" size="sm">
              Periodic job applications &amp; reporting
            </Text>
          </Group>
          <Badge variant="subtle" color="gray">
            polling every {POLL_INTERVAL_MS / 1000}s
          </Badge>
        </Group>

        {error && (
          <Alert color="red" title="API unavailable" variant="light">
            {error}
          </Alert>
        )}

        <Card withBorder>
          <Title order={3} mb="md">
            Jobs
          </Title>
          <Table>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>ID</Table.Th>
                <Table.Th>Title</Table.Th>
                <Table.Th>Company</Table.Th>
                <Table.Th>URL</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {jobs.map((job) => (
                <Table.Tr key={job.id}>
                  <Table.Td>{job.id}</Table.Td>
                  <Table.Td>{job.title}</Table.Td>
                  <Table.Td>{job.company}</Table.Td>
                  <Table.Td>
                    <Anchor href={job.url} target="_blank">
                      {job.url}
                    </Anchor>
                  </Table.Td>
                </Table.Tr>
              ))}
              {jobs.length === 0 && (
                <Table.Tr>
                  <Table.Td colSpan={4}>No jobs configured</Table.Td>
                </Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Card>

        <Card withBorder>
          <Title order={3} mb="md">
            Reports
          </Title>
          <Table>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Applied at</Table.Th>
                <Table.Th>Job</Table.Th>
                <Table.Th>Company</Table.Th>
                <Table.Th>Result</Table.Th>
                <Table.Th>Message</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {reports.map((r) => (
                <Table.Tr key={`${r.job_id}-${r.applied_at}`}>
                  <Table.Td>{new Date(r.applied_at).toLocaleString()}</Table.Td>
                  <Table.Td>{r.job_title}</Table.Td>
                  <Table.Td>{r.company}</Table.Td>
                  <Table.Td>
                    <Badge
                      color={r.success ? 'green' : 'red'}
                      variant="light"
                    >
                      {r.success ? 'success' : 'failed'}
                    </Badge>
                  </Table.Td>
                  <Table.Td>{r.message}</Table.Td>
                </Table.Tr>
              ))}
              {reports.length === 0 && (
                <Table.Tr>
                  <Table.Td colSpan={5}>No reports yet</Table.Td>
                </Table.Tr>
              )}
            </Table.Tbody>
          </Table>
        </Card>
      </Stack>
    </Container>
  )
}
