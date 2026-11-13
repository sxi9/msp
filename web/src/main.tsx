import { render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import './styles.css';
import { api } from './api.ts';
import { resetPresets } from './lib/presets.ts';
import { navigate, useRoute, type Route } from './router.ts';
import type { User } from './types.ts';
import { Layout } from './components/Layout.tsx';
import { Login } from './views/Login.tsx';
import { Fleet } from './views/Fleet.tsx';
import { Device } from './views/Device.tsx';
import { Alerts } from './views/Alerts.tsx';
import { Orgs } from './views/Orgs.tsx';
import { Org } from './views/Org.tsx';
import { Audit } from './views/Audit.tsx';
import { Users } from './views/Users.tsx';
import { Account } from './views/Account.tsx';

function page(route: Route, user: User) {
  switch (route.name) {
    case 'fleet': return <Fleet />;
    case 'device': return <Device id={route.id} user={user} />;
    case 'alerts': return <Alerts user={user} />;
    case 'orgs': return <Orgs user={user} />;
    case 'org': return <Org id={route.id} user={user} />;
    case 'audit': return <Audit />;
    case 'users': return user.role === 'admin' ? <Users user={user} /> : <p class="error">Admins only.</p>;
    case 'account': return <Account user={user} />;
    default: return <p class="error">Page not found. <a href="#/">Back to the fleet</a></p>;
  }
}

function App() {
  const route = useRoute();
  // undefined = session check in flight; null = not signed in.
  const [user, setUser] = useState<User | null | undefined>(undefined);
  useEffect(() => {
    api.me().then((r) => setUser(r.user), () => { setUser(null); navigate('#/login'); });
  }, []);
  if (route.name === 'login') {
    return <Login onLogin={(u) => { setUser(u); navigate('#/'); }} />;
  }
  if (user === undefined) return <div class="center muted">Loading…</div>;
  if (user === null) return <div class="center"><a href="#/login">Sign in</a></div>;
  const logout = async () => {
    try { await api.logout(); } finally { resetPresets(); setUser(null); navigate('#/login'); }
  };
  return <Layout user={user} route={route} onLogout={logout}>{page(route, user)}</Layout>;
}

// index.html ships a "bundle not built" placeholder inside #app; Preact's
// render() diffs against existing children rather than clearing them.
const root = document.getElementById('app')!;
root.textContent = '';
render(<App />, root);
