// Playbooks view: metadata comes from the selected collection's playbooks
// endpoint. Manual run controls were removed with the run endpoints.
describe('Playbooks', () => {
  beforeEach(() => {
    cy.login()
    cy.visit('/playbooks?org=default&collection=alpha')
    cy.contains('h4', 'Playbooks').should('be.visible')
  })

  const selectOrg = (name) =>
    cy.contains('label', 'Organization').parent().find('select').select(name)
  const selectCollection = (name) =>
    cy.contains('label', 'Collection').parent().find('select').select(name)

  it('lists playbooks with path, inventory and status', () => {
    cy.contains('td', 'web')
      .closest('tr')
      .should('contain.text', 'playbooks/web.yml')
      .and('contain.text', 'inventory/hosts')

    cy.contains('td', 'common')
      .closest('tr')
      .should('contain.text', 'playbooks/common.yml')
      .and('contain.text', 'inventory/hosts')
  })

  it('shows successful and failed statuses from the collection', () => {
    cy.contains('td', 'web').closest('tr').should('contain.text', 'ok')
    cy.contains('td', 'common').closest('tr').should('contain.text', 'ok')
    cy.contains('button', 'Run').should('not.exist')
    cy.contains('button', 'Check').should('not.exist')
  })

  it('switches to another collection and mirrors its playbooks in the sidebar', () => {
    selectOrg('lab')
    selectCollection('beta')

    cy.contains('td', 'databases').closest('tr').should('contain.text', 'failed')
    cy.contains('td', 'bootstrap').closest('tr').should('contain.text', 'never run')
    cy.get('nav').should('contain.text', 'databases').and('contain.text', 'bootstrap')
    cy.get('nav').should('not.contain.text', 'web')
  })
})
