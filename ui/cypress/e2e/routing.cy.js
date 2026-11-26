// Routing fallbacks: unknown paths land on the dashboard, unknown hosts get
// a dedicated "not found" state, and routes for removed endpoints are gone.
describe('Routing', () => {
  beforeEach(() => {
    cy.login()
  })

  it('redirects unknown paths to the dashboard', () => {
    cy.visit('/definitely-not-a-route')
    cy.location('pathname').should('eq', '/')
    cy.contains('h4', 'Collections').should('be.visible')
  })

  it('shows "host not found" for an unknown host', () => {
    cy.visit('/hosts/ghost-01?org=default&collection=alpha')
    cy.contains('Host not found')
    cy.contains('It may have been removed from the inventory.')
  })

  it('no longer has a run detail route', () => {
    cy.visit('/runs/3')
    cy.location('pathname').should('eq', '/')
    cy.contains('h4', 'Collections').should('be.visible')
  })

  it('does not serve removed or flattened API endpoints', () => {
    for (const path of [
      '/api/runs',
      '/api/v1/orgs/default/collections/alpha/hostkeys',
      '/api/v1/workers',
      '/api/v1/hosts/web-01/changes',
    ]) {
      cy.request({ url: path, failOnStatusCode: false }).its('status').should('eq', 404)
    }
  })
})
