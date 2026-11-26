// Dashboard: counts are derived from orgs, collection details, hosts and
// playbooks. There is no stats endpoint and no recent-runs table anymore.
describe('Dashboard', () => {
  beforeEach(() => {
    cy.login()
    cy.visit('/')
    cy.get('h4').contains('Dashboard').should('be.visible')
    cy.contains('h4', 'Collections').should('be.visible')
  })

  it('renders overview cards from collection data', () => {
    // 3 alpha hosts + 2 beta hosts; 4 in inventory, 4 active, 1 disappeared.
    cy.contains('p', 'Hosts').parent().contains('5')
    cy.contains('4 in inventory')
    cy.contains('4 active')
    cy.contains('1 disappeared')

    // 2 alpha playbooks + 2 beta playbooks, named from collection details.
    cy.contains('p', 'Playbooks').parent().contains('4')
    cy.contains('web, common, databases, bootstrap')

    // Two collections under default and lab.
    cy.contains('p', 'Collections').parent().contains('2')
    cy.contains('default, lab')

    // The failed-status count comes from the playbooks lists.
    cy.contains('p', 'Failed playbooks').parent().contains('1')
    cy.contains('last run')
  })

  it('lists each collection with host and playbook counts', () => {
    const alpha = () => cy.contains('td', 'alpha').closest('tr')
    const beta = () => cy.contains('td', 'beta').closest('tr')
    alpha().should('contain.text', 'default')
      .and('contain.text', '3')
      .and('contain.text', '2')
      .and('contain.text', 'web, common')
    beta().should('contain.text', 'lab')
      .and('contain.text', 'databases, bootstrap')

    cy.contains('h4', 'Recent runs').should('not.exist')
    cy.get('table tbody tr').should('have.length', 2)
  })

  it('navigates through the sidebar sections', () => {
    cy.get('[aria-label="Dashboard"]').click()
    cy.location('pathname').should('eq', '/')
    cy.get('[aria-label="Hosts"]').click()
    cy.location('pathname').should('eq', '/hosts')
    cy.get('[aria-label="Playbooks"]').click()
    cy.location('pathname').should('eq', '/playbooks')
    cy.get('[aria-label="Monitor"]').click()
    cy.location('pathname').should('eq', '/monitor')
    cy.get('[aria-label="Dashboard"]').click()
    cy.location('pathname').should('eq', '/')
  })

  it('keeps the URL stable when clicking dashboard anchor links', () => {
    cy.contains('a', 'Overview').click()
    cy.location('pathname').should('eq', '/')
    cy.contains('a', 'Collections').click()
    cy.location('pathname').should('eq', '/')
  })
})
