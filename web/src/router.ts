// Hash router: #/login, #/ (fleet), #/devices/:id, #/alerts, #/orgs, #/orgs/:id,
// #/notify, #/deliveries, #/software, #/scripts, #/jobs, #/automations, #/reports,
// #/audit, #/users, #/account.
import { useEffect, useState } from 'preact/hooks';

export type Route =
  | { name: 'login' } | { name: 'fleet' } | { name: 'device'; id: number } | { name: 'alerts' }
  | { name: 'orgs' } | { name: 'org'; id: number } | { name: 'notify' } | { name: 'deliveries' }
  | { name: 'software' } | { name: 'scripts' } | { name: 'jobs' } | { name: 'job'; id: number }
  | { name: 'automations' } | { name: 'reports' } | { name: 'audit' } | { name: 'users' }
  | { name: 'account' } | { name: 'notfound' };

export function parseRoute(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').filter(Boolean);
  const id = parts.length === 2 ? Number(parts[1]) : NaN;
  const withId = (name: 'device' | 'org' | 'job'): Route => (Number.isInteger(id) && id > 0 ? { name, id } : { name: 'notfound' });
  switch (parts[0]) {
    case undefined: return { name: 'fleet' };
    case 'login': return { name: 'login' };
    case 'devices': return withId('device');
    case 'alerts': return { name: 'alerts' };
    case 'orgs': return parts.length === 1 ? { name: 'orgs' } : withId('org');
    case 'notify': return { name: 'notify' };
    case 'deliveries': return { name: 'deliveries' };
    case 'software': return { name: 'software' };
    case 'scripts': return { name: 'scripts' };
    case 'jobs': return parts.length === 1 ? { name: 'jobs' } : withId('job');
    case 'automations': return { name: 'automations' };
    case 'reports': return { name: 'reports' };
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
