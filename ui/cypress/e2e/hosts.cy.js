// Hosts list: only the selected collection's hosts are shown; the org and
// collection selectors drive the scoped hosts endpoint.
describe('Hosts', () => {
  beforeEach(() => {
    cy.login()
    cy.visit('/hosts')
    cy.get('h4').contains('Hosts').should('be.visible')
    cy.contains('td', 'web-01')
  })

  const selectOrg = (name) =>
    cy.contains('label', 'Organization').parent().find('select').select(name)
  const selectCollection = (name) =>
    cy.contains('label', 'Collection').parent().find('select').select(name)

  it('lists the hosts of the default collection with their details', () => {
    for (const name of ['web-01', 'web-02', 'retired-01']) {
      cy.contains('td', name)
    }
    cy.contains('td', 'db-01').should('not.exist')
    cy.get('table tbody tr').should('have.length', 3)

    cy.contains('td', 'web-01')
      .closest('tr')
      .then((row) => {
        expect(row.text()).to.contain('10.0.1.10')
        expect(row.find('.mantine-Badge-root').text()).to.contain('webservers')
        expect(row.find('.mantine-Badge-root').text()).to.contain('changed')
      })
  })

  it('flags disappeared hosts', () => {
    cy.contains('td', 'retired-01')
      .closest('tr')
      .should('contain.text', 'disappeared')
  })

  it('filters active / disappeared / all', () => {
    cy.contains('span', 'active').click()
    cy.get('table tbody tr').should('have.length', 2)
    cy.contains('td', 'retired-01').should('not.exist')

    cy.contains('span', 'disappeared').click()
    cy.get('table tbody tr').should('have.length', 1)
    cy.contains('td', 'retired-01')

    cy.contains('span', 'all').click()
    cy.get('table tbody tr').should('have.length', 3)
  })

  it('filters hosts by name', () => {
    const search = 'input[placeholder="Search hosts by name"]'
    cy.get(search).type('web-')
    cy.get('table tbody tr').should('have.length', 2)
    cy.contains('td', 'retired-01').should('not.exist')

    cy.get(search).clear().type('DB-01')
    cy.get('table tbody tr').should('have.length', 1)
    cy.get('table tbody tr').should('contain.text', 'No hosts in this collection')

    cy.get(search).clear()
    cy.get('table tbody tr').should('have.length', 3)
  })

  it('switches collections and shows that collection hosts', () => {
    selectOrg('lab')
    selectCollection('beta')

    cy.contains('td', 'db-01')
    cy.contains('td', 'adhoc-01').closest('tr').should('contain.text', 'not in inventory')
    cy.contains('td', 'web-01').should('not.exist')
    cy.get('table tbody tr').should('have.length', 2)
    cy.location('search').should('contain', 'org=lab').and('contain', 'collection=beta')
  })

  it('opens a host from its row and from the sidebar', () => {
    cy.contains('td', 'web-02').click()
    cy.location('pathname').should('eq', '/hosts/web-02')
    cy.location('search').should('contain', 'collection=alpha')
    cy.go('back')
    cy.contains('a', 'db-01').should('not.exist')
    cy.contains('a', 'retired-01').click()
    cy.location('pathname').should('eq', '/hosts/retired-01')
  })
})
