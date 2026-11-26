// Shared e2e setup.
//
// The mock server enforces the same UI auth contract as the real server:
// a valid session cookie is required on every /api/v1/* endpoint except
// health, login, logout and me. Cypress' default test isolation clears
// cookies between tests, so each spec signs in via beforeEach.
Cypress.Commands.add('login', (username = 'admin', password = 'secret') => {
  cy.request('POST', '/api/v1/login', { username, password })
})
