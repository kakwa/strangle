// SSH view: pending host keys from every visible collection, approve/reject
// through the owning collection's URL, and master-level pool counters.
describe('SSH view', () => {
  beforeEach(() => {
    cy.request('POST', '/api/_mock/reset-hostkeys')
    cy.login()
  })

  it('lists pending keys and pool stats', () => {
    cy.visit('/ssh')
    cy.contains('h4', 'Pending host keys')
    cy.contains('td', 'alpha/web')
    cy.contains('td', '10.0.1.10')
    cy.contains('td', 'SHA256:mock-alpha-key')
    cy.contains('td', 'beta/databases')
    cy.contains('td', 'rejected')

    cy.contains('h4', 'SSH connection pools')
    cy.contains('td', 'alpha/web')
    cy.contains('td', '7') // total dials of alpha/web
  })

  it('approves a key through its collection', () => {
    cy.visit('/ssh')
    cy.contains('tr', '10.0.1.10').contains('button', 'Approve').click()
    cy.contains('td', '10.0.1.10').should('not.exist')
    cy.request('/api/v1/orgs/default/collections/alpha/hostkeys').then(({ body }) => {
      expect(body.map((k) => k.host)).not.to.include('10.0.1.10')
    })
  })

  it('rejects a key through its collection', () => {
    cy.visit('/ssh')
    cy.contains('tr', '10.0.2.20').contains('button', 'Reject').click()
    cy.contains('td', '10.0.2.20').should('not.exist')
    cy.request('/api/v1/orgs/lab/collections/beta/hostkeys').then(({ body }) => {
      expect(body.map((k) => k.host)).not.to.include('10.0.2.20')
    })
  })
})
