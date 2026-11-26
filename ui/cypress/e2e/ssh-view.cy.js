// SSH view: host keys are listed for one collection/playbook/host and are
// updated with a PATCH JSON-Patch document. The collection key list and SSH
// stats panels were removed.
describe('SSH view', () => {
  beforeEach(() => {
    cy.login()
    cy.request('POST', '/api/v1/_mock/reset-hostkeys')
    cy.visit('/ssh?org=default&collection=alpha')
    cy.contains('h4', 'Host keys').should('be.visible')
  })

  const selectOrg = (name) =>
    cy.contains('label', 'Organization').parent().find('select').select(name)
  const selectCollection = (name) =>
    cy.contains('label', 'Collection').parent().find('select').select(name)

  it('lists the keys of the selected playbook and host', () => {
    cy.contains('td', '10.0.1.10')
    cy.contains('td', 'SHA256:mock-alpha-key')
    cy.contains('td', 'pending')
    cy.contains('h4', 'SSH connection pools').should('not.exist')
  })

  it('approves a key with a PATCH JSON-Patch document', () => {
    cy.intercept(
      'PATCH',
      '/api/v1/orgs/default/collections/alpha/playbooks/web/hosts/10.0.1.10/hostkeys',
    ).as('approveKey')
    cy.contains('tr', '10.0.1.10').contains('button', 'Approve').click()
    cy.wait('@approveKey').then(({ request }) => {
      expect(request.body).to.deep.equal([
        { op: 'replace', path: '/status', value: 'approved' },
        { op: 'replace', path: '/fingerprint', value: 'SHA256:mock-alpha-key' },
      ])
    })
    cy.contains('tr', '10.0.1.10').should('contain.text', 'approved')
    cy.request(
      '/api/v1/orgs/default/collections/alpha/playbooks/web/hosts/10.0.1.10/hostkeys',
    ).then(({ body }) => {
      expect(body[0].status).to.eq('approved')
    })
  })

  it('rejects a key of another collection through its own scope', () => {
    cy.intercept(
      'PATCH',
      '/api/v1/orgs/lab/collections/beta/playbooks/databases/hosts/10.0.2.20/hostkeys',
    ).as('rejectKey')
    selectOrg('lab')
    selectCollection('beta')
    cy.contains('td', '10.0.2.20')

    cy.contains('tr', '10.0.2.20').contains('button', 'Reject').click()
    cy.wait('@rejectKey').then(({ request }) => {
      expect(request.body).to.deep.equal([
        { op: 'replace', path: '/status', value: 'rejected' },
      ])
    })
    cy.contains('tr', '10.0.2.20').should('contain.text', 'rejected')
  })
})
