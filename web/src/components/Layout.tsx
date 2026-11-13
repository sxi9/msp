import type { ComponentChildren } from 'preact';
import type { Route } from '../router.ts';
import type { User } from '../types.ts';
import { isAdmin } from '../types.ts';
import { api } from '../api.ts';
import { usePoll } from '../hooks.ts';

const NAV: { hash: string; label: string; match: Route['name'][]; admin?: boolean }[] = [
  { hash: '#/', label: 'Fleet', match: ['fleet', 'device'] },
  { hash: '#/alerts', label: 'Alerts', match: ['alerts'] },
  { hash: '#/orgs', label: 'Orgs', match: ['orgs', 'org'] },
  { hash: '#/audit', label: 'Audit', match: ['audit'] },
  { hash: '#/users', label: 'Users', match: ['users'], admin: true },
];

export function Layout({ user, route, onLogout, children }: { user: User; route: Route; onLogout: () => void; children: ComponentChildren }) {
  const summary = usePoll(() => api.summary(), [], 10000).data;
  return (
    <div class="app">
      <header class="topbar">
        <a class="brand" href="#/">MSP</a>
        <nav class="nav">
          {NAV.filter((n) => !n.admin || isAdmin(user)).map((n) => (
            <a key={n.hash} href={n.hash} class={n.match.includes(route.name) ? 'active' : ''}>
              {n.label}
              {n.label === 'Alerts' && summary && summary.open_alerts > 0 && <span class="badge badge-amber">{summary.open_alerts}</span>}
            </a>
          ))}
        </nav>
        {summary && (
          <div class="counts" title="orgs / devices online / offline / pending">
            <span>{summary.orgs} orgs</span>
            <span class="ok">{summary.online} online</span>
            <span class={summary.offline ? 'warn' : ''}>{summary.offline} offline</span>
            <span class="muted">{summary.pending} pending</span>
          </div>
        )}
        <div class="userbox">
          <span class="email" title={`${user.role}: account settings`}><a href="#/account" class={route.name === 'account' ? 'active' : ''}>{user.email}</a> <span class="chip">{user.role}</span></span>
          <button type="button" class="btn btn-sm" onClick={onLogout}>Log out</button>
        </div>
      </header>
      <main class="page">{children}</main>
    </div>
  );
}
