import { useMemo } from 'react'
import {
  IconApi,
  IconBook,
  IconFingerprint,
  IconGauge,
  IconKey,
  IconLogout,
  IconServer2,
} from '@tabler/icons-react'
import { NativeSelect, Text, UnstyledButton } from '@mantine/core'
import classes from './NavbarSegmented.module.css'

const sections = [
  { icon: IconGauge, label: 'Dashboard' },
  { icon: IconServer2, label: 'Hosts' },
  { icon: IconBook, label: 'Playbooks' },
  { icon: IconFingerprint, label: 'SSH' },
  { icon: IconKey, label: 'Tokens' },
  { icon: IconApi, label: 'API' },
]

export function NavbarSegmented({
  active,
  onSection,
  orgs,
  selectedOrg,
  onOrgChange,
  user,
  onLogout,
  collapsed = false,
}) {
  const activeSection = active.toLowerCase()
  const orgData = useMemo(
    () => (orgs || []).map((o) => ({ label: o.name, value: o.name })),
    [orgs],
  )

  return (
    <nav className={classes.navbar} data-collapsed={collapsed || undefined}>
      <div>
        {!collapsed && (
          <Text fw={500} size="sm" className={classes.title} c="dimmed" mb="xs">
            {user || 'Strangle'}
          </Text>
        )}

        {orgData.length > 0 && !collapsed && (
          <NativeSelect
            aria-label="Organization"
            value={selectedOrg || orgData[0]?.value}
            onChange={(event) => onOrgChange(event.currentTarget.value)}
            data={orgData}
          />
        )}
      </div>

      <div className={classes.navbarMain} style={{ marginTop: 'var(--mantine-spacing-md)' }}>
        {sections.map((item) => (
          <UnstyledButton
            key={item.label}
            className={classes.link}
            data-active={item.label.toLowerCase() === activeSection || undefined}
            aria-label={item.label}
            title={collapsed ? item.label : undefined}
            onClick={(event) => {
              event.preventDefault()
              onSection(item.label)
            }}
          >
            <item.icon className={classes.linkIcon} stroke={1.5} />
            {!collapsed && <span>{item.label}</span>}
          </UnstyledButton>
        ))}
      </div>

      <div className={classes.footer}>
        <a
          href="#"
          className={classes.link}
          onClick={(event) => {
            event.preventDefault()
            onLogout()
          }}
        >
          <IconLogout className={classes.linkIcon} stroke={1.5} />
          {!collapsed && <span>Logout</span>}
        </a>
      </div>
    </nav>
  )
}
