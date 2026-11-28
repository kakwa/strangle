import '@stoplight/elements/styles.min.css'
import '@stoplight/elements/web-components.min.js'

// Stoplight Elements renders an interactive API reference from the live
// OpenAPI document. `tryItCredentialsPolicy="include"` sends the UI session
// cookie with Try It requests, so protected operations work for signed-in
// users without extra configuration. The `hash` router keeps Elements'
// internal navigation separate from the app's React Router routes.
export default function ApiScreen() {
  return (
    <div
      data-testid="api-reference"
      style={{ height: 'calc(100vh - 140px)', minHeight: 520 }}
    >
      <elements-api
        apiDescriptionUrl="/api/v1/openapi.json"
        tryItCredentialsPolicy="include"
        layout="sidebar"
        router="hash"
        style={{ height: '100%' }}
      />
    </div>
  )
}
