// Hash router: #/login, #/ (fleet), #/devices/:id, #/alerts, #/orgs, #/orgs/:id, #/audit, #/users, #/account.
import { useEffect, useState } from 'preact/hooks';

export type Route =
  | { name: 'login' } | { name: 'fleet' } | { name: 'device'; id: number } | { name: 'alerts' }
  | { name: 'orgs' } | { name: 'org'; id: number } | { name: 'audit' } | { name: 'users' } | { name: 'account' } | { name: 'notfound' };

export function parseRoute(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').filter(Boolean);
  const id = parts.length === 2 ? Number(parts[1]) : NaN;
  const withId = (name: 'device' | 'org'): Route => (Number.isInteger(id) && id > 0 ? { name, id } : { name: 'notfound' });
  switch (parts[0]) {
    case undefined: return { name: 'fleet' };
    case 'login': return { name: 'login' };
    case 'devices': return withId('device');
    case 'alerts': return { name: 'alerts' };
    case 'orgs': return parts.length === 1 ? { name: 'orgs' } : withId('org');
    case 'audit': return { name: 'audit' };
    case 'users': return { name: 'users' };
    case 'account': return { name: 'account' };
    default: return { name: 'notfound' };
  }
}

export function navigate(hash: string): void {
  location.hash = hash;
}

export function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => parseRoute(location.hash));
  useEffect(() => {
    const on = () => setRoute(parseRoute(location.hash));
    addEventListener('hashchange', on);
    return () => removeEventListener('hashchange', on);
  }, []);
  return route;
}
