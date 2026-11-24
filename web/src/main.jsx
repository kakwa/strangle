import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { MantineProvider, createTheme } from '@mantine/core'
import '@mantine/core/styles.css'
import App from './App.jsx'
import './index.css'

const theme = createTheme({
  colorScheme: 'dark',
  fontFamily:
    'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
})

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <MantineProvider theme={theme}>
      <App />
    </MantineProvider>
  </StrictMode>,
)
