import { ColorSchemeScript } from '@mantine/core'

// Applies the persisted (or system) color scheme before React mounts so the
// app does not flash the wrong theme on load.
document.write(ColorSchemeScript.render({ defaultColorScheme: 'auto' }))
