// Run detail: the polling list links to a bookmarkable run view, and the
// full captured output is fetched only on the detail screen.
describe('Run detail', () => {
  beforeEach(() => {
    cy.login()
  })

  it('opens a run from the recent runs table', () => {
    cy.visit('/')
    cy.get('#dash-runs').contains('button', 'web').click()
    cy.location('pathname').should('eq', '/runs/3')
    cy.contains('Run #3').should('be.visible')
    cy.contains('Collection: alpha')
    cy.contains('Playbook: web')
    cy.contains('pre', 'TASK [Update Nginx config]')
    cy.contains('changed: [web-01]')
    cy.contains('Task results').should('be.visible')
    cy.contains('web-01: Update Nginx config').click()
    cy.contains('+    server_name web01.example.com;')
    cy.contains('Collect disk usage').click()
    cy.contains('df: command not found')
  })

  it('renders from a bookmarkable URL and navigates back', () => {
    cy.visit('/runs/4')
    cy.contains('Run #4').should('be.visible')
    cy.contains('pre', 'fatal: connection refused')
    cy.contains('← back to dashboard').click()
    cy.location('pathname').should('eq', '/')
  })

  it('shows an error for an unknown run', () => {
    cy.visit('/runs/999')
    cy.contains('Could not load the run')
  })
})
