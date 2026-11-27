// Host detail: a playbook selector drives the playbook/host-scoped runs URL,
// Ansible facts are shown, and the removed task-changes panel is absent.
describe('Host detail', () => {
  beforeEach(() => {
    cy.login()
  })

  const selectPlaybook = (name) =>
    cy.contains('label', 'Playbook').parent().find('select').select(name)

  it('renders runs for the selected playbook and the host facts', () => {
    cy.intercept(
      'GET',
      '/api/v1/orgs/default/collections/alpha/playbooks/web/hosts/web-01/runs?limit=10',
    ).as('webRuns')
    cy.visit('/hosts/web-01?org=default&collection=alpha')
    cy.wait('@webRuns')

    cy.get('h4').contains('web-01').should('be.visible')
    cy.contains('p', '← back to hosts')
    cy.contains('h4', 'Playbook runs')
    cy.get('table').contains('td', '#3')
    cy.get('table').contains('td', 'changed')
    cy.contains('h4', 'Ansible facts')
    cy.contains('pre', '"ansible_hostname": "web-01"')
    // Nested facts are collapsed by default and expand on click.
    cy.contains('"ansible_default_ipv4": {…1}')
      .should('not.contain', '"address"')
      .click()
    cy.contains('"address": "10.0.1.10"')
    cy.contains('h4', 'Task changes').should('not.exist')
  })

  it('refetches runs when another playbook is selected', () => {
    cy.intercept(
      'GET',
      '/api/v1/orgs/default/collections/alpha/playbooks/*/hosts/web-01/runs?limit=10',
    ).as('hostRuns')
    cy.visit('/hosts/web-01?org=default&collection=alpha')
    cy.wait('@hostRuns')
    cy.get('table').contains('td', '#3')

    selectPlaybook('common')
    cy.wait('@hostRuns')
    cy.get('table').contains('td', '#1')
    cy.get('table').contains('td', '#3').should('not.exist')
  })

  it('shows an empty state for a host without runs in the playbook', () => {
    cy.visit('/hosts/web-02?org=default&collection=alpha')
    cy.contains('No runs recorded for this host in web yet')
  })

  it('navigates back to the hosts list with the scope preserved', () => {
    cy.visit('/hosts/web-01?org=default&collection=alpha')
    cy.contains('p', '← back to hosts').click()
    cy.location('pathname').should('eq', '/hosts')
    cy.location('search').should('contain', 'collection=alpha')
  })
})
