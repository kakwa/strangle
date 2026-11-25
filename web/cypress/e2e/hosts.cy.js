// Hosts list: rows, inventory/disappeared badges, filters, navigation.
describe('Hosts', () => {
  beforeEach(() => {
    cy.visit('/hosts')
    cy.get('h4').contains('Hosts').should('be.visible')
  })

  it('lists every host once with its details', () => {
    for (const name of ['web-01', 'web-02', 'db-01', 'retired-01', 'adhoc-01']) {
      cy.contains('td', name)
    }
    // 5 distinct rows (hosts appearing in several targets are merged).
    cy.get('table tbody tr').should('have.length', 5)
  })

  it('shows IP, groups and status for a host', () => {
    cy.contains('td', 'web-01')
      .closest('tr')
      .then((row) => {
        expect(row.text()).to.contain('10.0.1.10')
        expect(row.find('.mantine-Badge-root').text()).to.contain('webservers')
        expect(row.find('.mantine-Badge-root').text()).to.contain('changed')
      })
  })

  it('flags hosts that are not in the inventory', () => {
    cy.contains('td', 'adhoc-01')
      .closest('tr')
      .should('contain.text', 'not in inventory')
  })

  it('flags disappeared hosts', () => {
    cy.contains('td', 'retired-01')
      .closest('tr')
      .should('contain.text', 'disappeared')
  })

  it('filters active / disappeared / all', () => {
    cy.contains('span', 'active').click()
    cy.get('table tbody tr').should('have.length', 4)
    cy.contains('td', 'retired-01').should('not.exist')

    cy.contains('span', 'disappeared').click()
    cy.get('table tbody tr').should('have.length', 1)
    cy.contains('td', 'retired-01')

    cy.contains('span', 'all').click()
    cy.get('table tbody tr').should('have.length', 5)
  })

  it('opens a host from its row and from the sidebar', () => {
    cy.contains('td', 'web-02').click()
    cy.location('pathname').should('eq', '/hosts/web-02')
    cy.go('back')
    cy.contains('a', 'db-01').click()
    cy.location('pathname').should('eq', '/hosts/db-01')
  })
})
