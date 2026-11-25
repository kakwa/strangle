// API token self-service: a signed-in user can list, create and revoke their
// bearer tokens. The raw value of a new token is shown exactly once in a
// copyable alert; only a prefix appears in the list.
describe('Tokens', () => {
  beforeEach(() => {
    cy.login()
    cy.visit('/tokens')
    cy.contains('Your API tokens').should('be.visible')
  })

  it('shows the empty state with no tokens', () => {
    cy.contains('No tokens yet — create one above.')
  })

  it('creates a token, shows the raw value once, lists the prefix, and revokes', () => {
    // Create it.
    cy.get('input[placeholder="e.g. ci, laptop"]').type('ci')
    cy.contains('button', 'Create token').click()

    // The raw value is shown exactly once, in the creation alert. The
    // description card renders three <code> elements, so scope to the last
    // one on the page, which is the copyable raw value.
    cy.contains('Token ci created')
    cy.get('code').last().should(($code) => {
      expect($code.text()).to.match(/^st_[0-9a-f]{64}$/)
    })

    // The list shows the name and the prefix (not the full value).
    cy.contains('td', 'ci')
      .closest('tr')
      .should('contain.text', 'st_')
      .and('contain.text', 'never used')

    // Revoking removes it.
    cy.contains('td', 'ci')
      .closest('tr')
      .contains('button', 'Revoke')
      .click()
    cy.contains('No tokens yet — create one above.')
  })

  it('rejects a duplicate token name', () => {
    cy.get('input[placeholder="e.g. ci, laptop"]').type('dup')
    cy.contains('button', 'Create token').click()
    cy.contains('Token dup created')

    // Same name again: the server's 409 message is surfaced.
    cy.get('input[placeholder="e.g. ci, laptop"]').type('dup')
    cy.contains('button', 'Create token').click()
    cy.contains('Token request failed')
    cy.contains('token "dup" already exists')
  })
})
