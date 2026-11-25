// Dashboard: stat cards, disappeared hosts, recent runs, per-playbook,
// sidebar navigation and anchor links.
describe('Dashboard', () => {
  beforeEach(() => {
    cy.visit('/')
    cy.get('h4').contains('Dashboard').should('be.visible')
  })

  it('renders the overview stat cards', () => {
    // Hosts card: total 5, 4 in inventory, 4 active, 1 disappeared.
    cy.contains('p', 'Hosts').parent().contains('5')
    cy.contains('4 in inventory')
    cy.contains('4 active')
    cy.contains('1 disappeared')

    // Runs card: 4 runs, 3 succeeded, 1 failed, an average duration.
    cy.contains('p', 'Runs').parent().contains('4')
    cy.contains('3 succeeded')
    cy.contains('1 failed')
    cy.contains('Avg duration')
    cy.contains('last run')

    // Playbooks card: 4 playbooks, named in the caption.
    cy.contains('p', 'Playbooks').parent().contains('4')
    cy.contains('web, common, databases, bootstrap')
  })

  it('lists the disappeared hosts and opens one', () => {
    cy.contains('h4', 'Disappeared hosts').should('be.visible')
    cy.contains('span', 'retired-01').click()
    cy.location('pathname').should('eq', '/hosts/retired-01')
  })

  it('renders the recent runs table', () => {
    cy.contains('h4', 'Recent runs')
    cy.get('table').contains('td', 'databases')
    cy.get('table').contains('td', 'web')
    cy.get('table').contains('td', 'exit 2')
    cy.get('table').contains('td', 'ok')
  })

  it('renders the per-playbook table', () => {
    cy.contains('h4', 'Per-playbook')
    cy.contains('td', 'databases').closest('tr').should('contain.text', '1')
    cy.contains('td', 'common').closest('tr').should('contain.text', '2')
  })

  it('navigates through the sidebar sections', () => {
    cy.get('[aria-label="Dashboard"]').click()
    cy.location('pathname').should('eq', '/')
    cy.get('[aria-label="Hosts"]').click()
    cy.location('pathname').should('eq', '/hosts')
    cy.get('[aria-label="Playbooks"]').click()
    cy.location('pathname').should('eq', '/playbooks')
    cy.get('[aria-label="Dashboard"]').click()
    cy.location('pathname').should('eq', '/')
  })

  it('keeps the URL stable when clicking dashboard anchor links', () => {
    cy.contains('a', 'Overview').click()
    cy.location('pathname').should('eq', '/')
    cy.contains('a', 'Recent runs').click()
    cy.location('pathname').should('eq', '/')
    cy.contains('a', 'Per-playbook').click()
    cy.location('pathname').should('eq', '/')
  })
})
