// UI auth: an unauthenticated visitor sees the login screen, a wrong
// password shows the server's error, and a successful login reveals the
// app with the signed-in user and a log-out button in the header.
describe('Auth', () => {
  it('shows the login screen without a session', () => {
    cy.visit('/')
    cy.contains('h3', 'strangle')
    cy.contains('label', 'Username')
    cy.contains('label', 'Password')
    cy.contains('button', 'Sign in')
  })

  it('rejects a wrong password', () => {
    cy.visit('/')
    cy.get('input').eq(0).type('admin')
    cy.get('input').eq(1).type('nope')
    cy.contains('button', 'Sign in').click()
    cy.contains('Sign in failed')
    cy.contains('invalid username or password')
    // still on the login screen
    cy.contains('button', 'Sign in')
  })

  it('logs in, shows the user in the header, and logs out', () => {
    cy.visit('/')
    cy.get('input').eq(0).type('admin')
    cy.get('input').eq(1).type('secret')
    cy.contains('button', 'Sign in').click()

    cy.get('h4').contains('Dashboard').should('be.visible')
    cy.contains('p', 'admin') // the username in the header
    cy.contains('button', 'Log out')

    cy.contains('button', 'Log out').click()
    cy.contains('button', 'Sign in')
  })
})
