// Monitor tab: worker information is parsed client-side from the Prometheus
// text served by /metrics; no API route is involved.
describe('Monitor', () => {
  beforeEach(() => {
    cy.login()
    cy.request('POST', '/api/v1/_mock/reset-workers')
  })

  it('is reachable from the sidebar and reads /metrics', () => {
    cy.intercept('GET', '/metrics').as('metrics')
    cy.visit('/')
    cy.get('[aria-label="Monitor"]').click()
    cy.location('pathname').should('eq', '/monitor')
    cy.wait('@metrics')

    cy.contains('h4', 'Workers')
    cy.contains('p', 'Total').parent().contains('2')
    cy.contains('p', 'Alive').parent().contains('1')
    cy.contains('p', 'Dead').parent().contains('1')

    cy.contains('tr', 'alpha').should('contain.text', '1').and('contain.text', '3')
    cy.contains('tr', 'beta').should('contain.text', '0').and('contain.text', '0')
  })

  it('renders the raw metric text in the worker table', () => {
    cy.request('POST', '/api/v1/_mock/set-workers', {
      workers: [{ worker: '7', collection: 'edge', jobs: 4, restarts: 9, alive: true }],
    })
    cy.visit('/monitor')
    cy.contains('tr', '7').should('contain.text', 'edge')
      .and('contain.text', '4')
      .and('contain.text', '9')
    cy.contains('p', 'Total').parent().contains('1')
  })

  it('shows a clear empty state when no workers are present', () => {
    cy.request('POST', '/api/v1/_mock/set-workers', { workers: [] })
    cy.visit('/monitor')
    cy.contains('No workers are registered.')
    cy.contains('p', 'Total').parent().contains('0')
  })
})
