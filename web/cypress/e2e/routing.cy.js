// Routing fallbacks: unknown paths land on the dashboard, unknown hosts get
// a dedicated "not found" state instead of a broken view.
describe('Routing', () => {
  beforeEach(() => {
    cy.login()
  })

  it('redirects unknown paths to the dashboard', () => {
    cy.visit('/definitely-not-a-route')
    cy.location('pathname').should('eq', '/')
    cy.contains('h4', 'Recent runs').should('be.visible')
  })

  it('shows "host not found" for an unknown host', () => {
    cy.visit('/hosts/ghost-01')
    cy.contains('Host not found')
    cy.contains('It may have been removed from the inventory.')
  })
})
