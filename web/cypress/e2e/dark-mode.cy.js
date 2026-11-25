// Dark mode: the header toggle flips the theme and the choice persists
// across reloads (stored under the same key the inline index.html script
// reads: mantine-color-scheme-value).
describe('Dark mode', () => {
  before(() => {
    cy.clearLocalStorage()
  })

  it('toggles the theme and persists it across a reload', () => {
    cy.visit('/')

    // Fresh profile, system preference is light in headless Chrome.
    cy.get('html').should('have.attr', 'data-mantine-color-scheme', 'light')

    cy.get('[aria-label="Toggle dark mode"]').click()
    cy.get('html').should('have.attr', 'data-mantine-color-scheme', 'dark')

    // After a reload the inline script applies the persisted scheme before
    // React mounts.
    cy.reload()
    cy.get('html').should('have.attr', 'data-mantine-color-scheme', 'dark')

    // Switch back so later tests start in light mode.
    cy.get('[aria-label="Toggle dark mode"]').click()
    cy.get('html').should('have.attr', 'data-mantine-color-scheme', 'light')
  })
})
