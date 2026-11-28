import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { MantineProvider, createTheme } from '@mantine/core'
import '@mantine/core/styles.css'
import { BrowserRouter } from 'react-router-dom'
import App from './App.jsx'
import './index.css'

const theme = createTheme({
  fontFamily:
    'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  // Respect the OS-level reduced-motion preference so animated components
  // (e.g. accordion panels) render instantly for users who opt out of motion.
  respectReducedMotion: true,
})

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <BrowserRouter>
      <MantineProvider theme={theme} defaultColorScheme="auto">
        <App />
      </MantineProvider>
    </BrowserRouter>
  </StrictMode>,
)
