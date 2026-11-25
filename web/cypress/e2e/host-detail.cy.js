// Host detail: bookmarkable URL, runs table, task changes accordion with
// colored diffs and errors, back navigation.
describe('Host detail', () => {
  beforeEach(() => {
    cy.login()
  })

  it('renders from a bookmarkable URL', () => {
    cy.visit('/hosts/web-01')
    cy.get('h4').contains('web-01').should('be.visible')
    cy.contains('← back to hosts')
    cy.contains('h4', 'Last 2 runs')
    cy.get('table').contains('td', '#3')
    cy.get('table').contains('td', '#1')
    cy.get('table').contains('td', 'changed')
    cy.get('table').contains('td', 'ok')
  })

  it('shows a colored diff for a changed task', () => {
    cy.visit('/hosts/web-01')
    cy.contains('h4', 'Task changes')

    cy.contains('p', 'Update Nginx config')
      .closest('.mantine-Accordion-control')
      .click()
    cy.contains('last run:')
    cy.contains('last changed:')

    // The diff renders in a monospace block with per-line colors.
    cy.contains('pre', 'server_name web01.example.com')
    cy.contains('pre', 'server_name old.example.com')
    cy.get('pre').first().then(($pre) => {
      const colors = new Set()
      for (const span of $pre.find('span')) {
        const c = Cypress.$(span).css('color')
        // Skip lines without an explicit color (context lines) and the
        // default text color, keep the diff-specific ones.
        if (c && c !== 'rgb(0, 0, 0)' && c !== 'rgb(255, 255, 255)') {
          colors.add(c)
        }
      }
      // Added (green), removed (red), hunk header (blue) and file header
      // (gray) lines all carry distinct colors.
      expect(colors.size, 'at least 2 distinct line colors').to.be.gte(2)
    })
  })

  it('shows the error block for a failed task', () => {
    cy.visit('/hosts/web-01')
    cy.contains('p', 'Collect disk usage')
      .closest('.mantine-Accordion-control')
      .click()
    cy.contains('pre', 'df: command not found')
  })

  it('shows empty states for a host without runs or changes', () => {
    cy.visit('/hosts/web-02')
    cy.contains('No task changes recorded for this host yet')
    cy.contains('No runs recorded for this host yet')
  })

  it('navigates back to the hosts list', () => {
    cy.visit('/hosts/web-01')
    cy.contains('p', '← back to hosts').click()
    cy.location('pathname').should('eq', '/hosts')
  })
})
