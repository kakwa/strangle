import {
  IconApi,
  IconBook,
  IconFingerprint,
  IconGauge,
  IconKey,
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
import { Link as RouterLink } from 'react-router-dom'
import classes from './DoubleNavbar.module.css'

const mainLinks = [
  { icon: IconGauge, label: 'Dashboard' },
  { icon: IconServer2, label: 'Hosts' },
  { icon: IconBook, label: 'Playbooks' },
  { icon: IconFingerprint, label: 'SSH' },
  { icon: IconKey, label: 'Tokens' },
  { icon: IconApi, label: 'API' },
]

const dashboardLinks = [
  { id: 'dash-overview', label: 'Overview' },
  { id: 'dash-collections', label: 'Collections' },
]

export function DoubleNavbar({
  active,
  onSection,
  hosts,
  selectedHost,
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
    <RouterLink
      className={classes.link}
      data-active={selectedHost === h.name || undefined}
      to={`/hosts/${encodeURIComponent(h.name)}`}
      key={h.name}
    >
      {h.name}
    </RouterLink>
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
      {activeSection !== 'api' && (
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
                {hostLinks.length === 0 && (
                  <span className={classes.link} style={{ cursor: 'default' }}>
                    No hosts in this collection
                  </span>
                )}
                {hostLinks}
              </ScrollArea.Autosize>
            )}
            {activeSection === 'playbooks' && (
              <ScrollArea.Autosize mah="100%">
                {playbooks.length === 0 && (
                  <span className={classes.link} style={{ cursor: 'default' }}>
                    No playbooks in this collection
                  </span>
                )}
                {playbooks.map((pb) => (
                  <span key={pb.name} className={classes.link} style={{ cursor: 'default' }}>
                    {pb.name}
                  </span>
                ))}
              </ScrollArea.Autosize>
            )}
          </div>
        </div>
      )}
    </nav>
  )
}
