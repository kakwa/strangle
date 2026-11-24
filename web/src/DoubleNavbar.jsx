import {
  IconBook,
  IconGauge,
  IconServer,
  IconServer2,
} from '@tabler/icons-react'
import {
  Anchor,
  ScrollArea,
  ThemeIcon,
  Title,
  Tooltip,
  UnstyledButton,
} from '@mantine/core'
import classes from './DoubleNavbar.module.css'

const mainLinks = [
  { icon: IconGauge, label: 'Dashboard' },
  { icon: IconServer2, label: 'Hosts' },
  { icon: IconBook, label: 'Playbooks' },
]

const dashboardLinks = [
  { id: 'dash-overview', label: 'Overview' },
  { id: 'dash-runs', label: 'Recent runs' },
  { id: 'dash-playbooks', label: 'Per-playbook' },
]

export function DoubleNavbar({
  active,
  onSection,
  hosts,
  selectedHost,
  onSelectHost,
  playbooks,
}) {
  const activeSection = active.toLowerCase()
  const mainLinksNodes = mainLinks.map((link) => (
    <Tooltip
      label={link.label}
      position="right"
      withArrow
      transitionProps={{ duration: 0 }}
      key={link.label}
    >
      <UnstyledButton
        onClick={() => onSection(link.label)}
        className={classes.mainLink}
        data-active={link.label.toLowerCase() === activeSection || undefined}
        aria-label={link.label}
      >
        <link.icon size={22} stroke={1.5} />
      </UnstyledButton>
    </Tooltip>
  ))

  const hostLinks = hosts.map((h) => (
    <a
      className={classes.link}
      data-active={selectedHost === h.name || undefined}
      href="#"
      onClick={(event) => {
        event.preventDefault()
        onSelectHost(h.name)
      }}
      key={h.name}
    >
      {h.name}
    </a>
  ))

  return (
    <nav className={classes.wrapper}>
      <div className={classes.aside}>
        <div className={classes.logo}>
          <ThemeIcon size={34} variant="light" color="blue">
            <IconServer size={18} stroke={1.5} />
          </ThemeIcon>
        </div>
        {mainLinksNodes}
      </div>
      <div className={classes.main}>
        <Title order={4} className={classes.title}>
          {activeSection === 'hosts' ? (selectedHost || 'Hosts') : active}
        </Title>
        <div className={classes.links}>
          {activeSection === 'dashboard' &&
            dashboardLinks.map((link) => (
              <Anchor
                key={link.id}
                className={classes.link}
                href={`#${link.id}`}
                onClick={(event) => {
                  event.preventDefault()
                  document
                    .getElementById(link.id)
                    ?.scrollIntoView({ behavior: 'smooth', block: 'start' })
                }}
              >
                {link.label}
              </Anchor>
            ))}
          {activeSection === 'hosts' && (
            <ScrollArea.Autosize mah="100%">
              {hostLinks}
            </ScrollArea.Autosize>
          )}
          {activeSection === 'playbooks' && (
            <ScrollArea.Autosize mah="100%">
              {Object.keys(playbooks).length === 0 && (
                <span className={classes.link} style={{ cursor: 'default' }}>
                  No targets configured
                </span>
              )}
              {Object.entries(playbooks).map(([target, pbs]) => (
                <div key={target}>
                  <span
                    className={classes.link}
                    style={{
                      cursor: 'default',
                      fontWeight: 700,
                      color: 'var(--mantine-color-dimmed)',
                      fontSize: 'var(--mantine-font-size-xs)',
                      textTransform: 'uppercase',
                    }}
                  >
                    {target}
                  </span>
                  {pbs.map((pb) => (
                    <span key={pb} className={classes.link} style={{ cursor: 'default', paddingLeft: 20 }}>
                      {pb}
                    </span>
                  ))}
                </div>
              ))}
            </ScrollArea.Autosize>
          )}
        </div>
      </div>
    </nav>
  )
}
