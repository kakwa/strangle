// The e2e suite runs against a local mock of the strangle API (see
// cypress/support/mock-server.mjs), which serves the production build in
// dist/ together with fixture data shaped exactly like the real API
// responses. The suite is therefore hermetic: no ansible, no postgres, no
// worker processes.
//
// Cypress 16 removed the e2e `webServer` config option, so the mock server
// is started and stopped by cypress/support/run-e2e.mjs (npm run test:e2e)
// before `cypress run` is invoked.
import { defineConfig } from 'cypress'

export default defineConfig({
  e2e: {
    baseUrl: 'http://127.0.0.1:18090',
    supportFile: 'cypress/support/e2e.js',
    specPattern: 'cypress/e2e/**/*.cy.js',
    video: false,
    screenshotOnRunFailure: false,
    viewportWidth: 1440,
    viewportHeight: 900,
    defaultCommandTimeout: 10000,
  },
})
