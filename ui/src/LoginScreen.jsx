import { useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Center,
  Stack,
  TextInput,
  Title,
} from '@mantine/core'
import { login } from './auth'

// LoginScreen is shown when the server enforces UI authentication and no
// valid session cookie is present. onSuccess receives the me response from
// the login endpoint.
export default function LoginScreen({ onSuccess }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError(null)
    try {
      onSuccess(await login(username, password))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Center h="100vh" p="md">
      <Card withBorder w={380} maw="100%">
        <form onSubmit={submit}>
          <Stack gap="md" p="lg">
            <Title order={3} ta="center">strangle</Title>
            {error && (
              <Alert color="red" title="Sign in failed">
                {error}
              </Alert>
            )}
            <TextInput
              label="Username"
              value={username}
              onChange={(e) => setUsername(e.currentTarget.value)}
              autoFocus
              required
            />
            <TextInput
              label="Password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
              required
            />
            <Button type="submit" loading={busy} disabled={busy}>
              Sign in
            </Button>
          </Stack>
        </form>
      </Card>
    </Center>
  )
}
