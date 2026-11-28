// API token self-service is collection-scoped: list/create/revoke all use
// /api/v1/orgs/{org}/collections/{collection}/tokens. The raw value of a new
// token is shown exactly once in a copyable alert.
describe('Tokens', () => {
  beforeEach(() => {
    cy.login()
    cy.request('POST', '/api/v1/_mock/reset-tokens')
    cy.intercept('GET', '/api/v1/orgs/default/collections/alpha/tokens').as('alphaTokens')
    cy.visit('/tokens?org=default&collection=alpha')
    cy.wait('@alphaTokens')
    cy.contains('Your API tokens').should('be.visible')
  })

  const selectOrg = (name) =>
    cy.get('nav select[aria-label="Organization"]').select(name)
  const selectCollection = (name) =>
    cy.contains('label', 'Collection').parent().find('select').select(name)

  it('shows the empty state with no tokens', () => {
    cy.contains('No tokens yet — create one above.')
  })

  it('creates a token in the selected collection, shows the raw value once, and revokes', () => {
    cy.intercept('POST', '/api/v1/orgs/default/collections/alpha/tokens').as('createToken')
    cy.get('input[placeholder="e.g. ci, laptop"]').type('ci')
    cy.contains('button', 'Create token').click()
    cy.wait('@createToken').its('request.body').should('deep.equal', { name: 'ci', ttl_hours: 0 })

    cy.contains('Token ci created')
    cy.get('code').last().should(($code) => {
      expect($code.text()).to.match(/^st_[0-9a-f]{64}$/)
    })

    cy.contains('td', 'ci')
      .closest('tr')
      .should('contain.text', 'st_')
      .and('contain.text', 'never used')

    cy.intercept('DELETE', '/api/v1/orgs/default/collections/alpha/tokens/ci').as('deleteToken')
    cy.contains('td', 'ci').closest('tr').contains('button', 'Revoke').click()
    cy.wait('@deleteToken')
    cy.contains('No tokens yet — create one above.')
  })

  it('rejects a duplicate token name in the same collection', () => {
    cy.get('input[placeholder="e.g. ci, laptop"]').type('dup')
    cy.contains('button', 'Create token').click()
    cy.contains('Token dup created')

    cy.get('input[placeholder="e.g. ci, laptop"]').type('dup')
    cy.contains('button', 'Create token').click()
    cy.contains('Token request failed')
    cy.contains('token "dup" already exists')
  })

  it('isolates tokens by collection', () => {
    cy.get('input[placeholder="e.g. ci, laptop"]').type('scoped')
    cy.contains('button', 'Create token').click()
    cy.contains('Token scoped created')

    selectOrg('lab')
    selectCollection('beta')
    cy.contains('No tokens yet — create one above.')

    selectOrg('default')
    selectCollection('alpha')
    cy.contains('td', 'scoped')
  })
})
