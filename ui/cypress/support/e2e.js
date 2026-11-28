// Shared e2e setup.
//
// The mock server enforces the same UI auth contract as the real server:
// a valid session cookie is required on every /api/v1/* endpoint except
// health, login, logout and me. Cypress' default test isolation clears
// cookies between tests, so each spec signs in via beforeEach.
Cypress.Commands.add('login', (username = 'admin', password = 'secret') => {
  cy.request('POST', '/api/v1/login', { username, password })
})

// Headless test browsers (Electron) do not fire requestAnimationFrame, so
// Mantine's Collapse/Accordion height animation never runs and panels stay
// collapsed (height:0), hiding their content from assertions. Forcing the
// reduced-motion preference makes those components render instantly
// (duration 0) instead of animating, keeping the suite deterministic. The
// patch must be applied per navigation (cy.visit resets the page context),
// hence the visit wrapper rather than a one-time support-file override.
function forceReducedMotion(win) {
  const native = win.matchMedia.bind(win)
  win.matchMedia = (query) => {
    const mql = native(query)
    if (typeof query === 'string' && query.includes('prefers-reduced-motion')) {
      return {
        ...mql,
        matches: true,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
        dispatchEvent: () => false,
      }
    }
    return mql
  }
}

Cypress.Commands.add('visitApp', (url, options = {}) => {
  return cy.visit(url, {
    ...options,
    onBeforeLoad(win) {
      forceReducedMotion(win)
      options.onBeforeLoad?.(win)
    },
  })
})
