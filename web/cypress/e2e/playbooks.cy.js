// Playbooks view: per-target cards, playbook metadata, manual run lifecycle
// (pending state resolves once the run is persisted and the next poll picks
// it up).
describe('Playbooks', () => {
  beforeEach(() => {
    cy.login()
    cy.visit('/playbooks')
    cy.contains('h4', 'alpha').should('be.visible')
    cy.contains('h4', 'beta').should('be.visible')
  })

  it('lists playbooks with path, inventory and status', () => {
    // web: name, monospace path, inventory.
    cy.contains('td', 'web')
      .closest('tr')
      .should('contain.text', 'playbooks/web.yml')
      .and('contain.text', 'inventory/hosts')

    // databases: last status failed.
    cy.contains('td', 'databases')
      .closest('tr')
      .should('contain.text', 'failed')

    // bootstrap: never run.
    cy.contains('td', 'bootstrap')
      .closest('tr')
      .should('contain.text', 'never run')
  })

  it('mirrors the playbook tree in the sidebar', () => {
    cy.get('nav').should('contain.text', 'alpha')
    cy.get('nav').should('contain.text', 'bootstrap')
    cy.get('nav').should('contain.text', 'web')
  })

  it('runs a playbook and resolves the pending state on the next poll', () => {
    const row = () => cy.contains('td', 'bootstrap').closest('tr')
    row().contains('button', 'Run').click()

    // The button immediately switches to its pending state.
    row().find('button').should('contain.text', 'Running…')

    // The mock persists the run at once; the UI resolves the pending state
    // on its next 10s poll.
    row().find('button').should(($btn) => {
      expect($btn.text()).to.not.contain('Running')
    }, { timeout: 20000 })

    // The new run shows up in the dashboard recent-runs table (4 → 5 rows).
    cy.visit('/')
    cy.get('table').first().find('tbody tr').should('have.length', 5)
  })
})
