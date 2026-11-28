// The API reference screen embeds Stoplight Elements, which fetches the live
// OpenAPI document from /api/v1/openapi.json and lets signed-in users call the
// API through its Try It console.
describe('API reference', () => {
  beforeEach(() => {
    cy.login()
    cy.intercept('GET', '/api/v1/openapi.json').as('spec')
    cy.visitApp('/api-spec')
    cy.wait('@spec')
    cy.get('elements-api', { timeout: 20000 }).should('exist')
  })

  it('renders the live document with Stoplight Elements', () => {
    cy.get('elements-api').should(($el) => {
      expect($el.prop('apiDescriptionUrl')).to.equal('/api/v1/openapi.json')
      expect($el.prop('tryItCredentialsPolicy')).to.equal('include')
    })
    cy.get('elements-api').contains('Strangle API').should('be.visible')
    cy.get('elements-api').contains('powered by Stoplight').should('be.visible')
    cy.get('elements-api').contains('system').click()
    cy.get('elements-api').contains('Liveness probe').should('be.visible')
  })

  it('sends a request through the Try It console', () => {
    cy.intercept('GET', '/api/v1/health').as('health')
    cy.get('elements-api').contains('system').click()
    cy.get('elements-api').contains('Liveness probe').click()
    cy.get('elements-api').contains('Send API Request').should('be.visible').click()
    cy.wait('@health').its('response.statusCode').should('eq', 200)
  })
})
