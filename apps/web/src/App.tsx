import { lazy, Suspense, useEffect, useState } from 'react';
import { Avatar, Button, Theme } from '@radix-ui/themes';
import { DashboardIcon, GearIcon, QuestionMarkCircledIcon, DesktopIcon, CubeIcon, ActivityLogIcon, MoonIcon, SunIcon, ExitIcon } from '@radix-ui/react-icons';
import { api, rememberAuth } from './api';
import type { AuthStatus } from './api';
import { Auth } from './Auth';
import { allowNavigation, Loading, Notice, useAction } from './ui';

const Overview = lazy(() => import('./Pages').then(module => ({ default: module.Overview })));
const Nodes = lazy(() => import('./Pages').then(module => ({ default: module.Nodes })));
const AddNode = lazy(() => import('./Pages').then(module => ({ default: module.AddNode })));
const NodeDetail = lazy(() => import('./Pages').then(module => ({ default: module.NodeDetail })));
const ServerList = lazy(() => import('./Pages').then(module => ({ default: module.ServerList })));
const Jobs = lazy(() => import('./Pages').then(module => ({ default: module.Jobs })));
const JobDetail = lazy(() => import('./Pages').then(module => ({ default: module.JobDetail })));
const ServerWizard = lazy(() => import('./ServerWizard').then(module => ({ default: module.ServerWizard })));
const ServerDetail = lazy(() => import('./ServerDetail').then(module => ({ default: module.ServerDetail })));
const Settings = lazy(() => import('./Settings').then(module => ({ default: module.Settings })));
const Help = lazy(() => import('./Settings').then(module => ({ default: module.Help })));

const nav = [{ path: 'overview', label: 'Обзор', icon: DashboardIcon }, { path: 'servers', label: 'Серверы', icon: CubeIcon }, { path: 'nodes', label: 'Хосты', icon: DesktopIcon }, { path: 'jobs', label: 'Задания', icon: ActivityLogIcon }, { path: 'settings', label: 'Настройки', icon: GearIcon }, { path: 'help', label: 'Помощь', icon: QuestionMarkCircledIcon }];
export default function App() {
  const [auth, setAuth] = useState<AuthStatus>(); const [authError, setAuthError] = useState('');
  const [route, setRoute] = useState(location.hash.slice(1) || '/overview');
  const [theme, setTheme] = useState<'light' | 'dark'>(() => localStorage.getItem('mp-theme') === 'dark' ? 'dark' : 'light');
  const action = useAction();
  const loadAuth = () => { setAuthError(''); api<AuthStatus>('/auth/status').then(status => { rememberAuth(status); setAuth(status); }).catch(e => setAuthError(e.message)); };
  useEffect(() => { loadAuth(); let previous = location.hash.slice(1) || '/overview'; const expired = () => { setAuth(undefined); loadAuth(); }; const hash = () => { const next = location.hash.slice(1) || '/overview'; if (next === previous) return; if (!allowNavigation()) { history.replaceState(null, '', `#${previous}`); return; } previous = next; setRoute(next); }; window.addEventListener('hashchange', hash); window.addEventListener('mp:session-expired', expired); return () => { window.removeEventListener('hashchange', hash); window.removeEventListener('mp:session-expired', expired); }; }, []);
  useEffect(() => { localStorage.setItem('mp-theme', theme); document.documentElement.style.colorScheme = theme; }, [theme]);
  const parts = route.split('/').filter(Boolean); const section = parts[0] || 'overview';
  const canManage = auth?.user?.role === 'owner' || auth?.user?.role === 'admin';
  const owner = auth?.user?.role === 'owner';
  const operator = auth?.user?.role === 'operator';
  const onAuth = (status: AuthStatus) => { rememberAuth(status); setAuth(status); };
  let page;
  if (section === 'overview') page = <Overview canManage={canManage} owner={owner} operator={operator} />;
  else if (section === 'nodes') page = operator ? <ServerList canManage={false} operator /> : parts[1] === 'new' && owner ? <AddNode /> : parts[1] && parts[2] === 'connect' && owner ? <AddNode id={parts[1]} /> : parts[1] ? <NodeDetail id={parts[1]} owner={owner} /> : <Nodes owner={owner} />;
  else if (section === 'servers') page = parts[1] === 'new' ? canManage ? <ServerWizard /> : <ServerList canManage={false} operator={operator} /> : parts[1] ? <ServerDetail id={parts[1]} user={auth?.user} /> : <ServerList canManage={canManage} operator={operator} />;
  else if (section === 'jobs') page = parts[1] ? <JobDetail id={parts[1]} /> : <Jobs />;
  else if (section === 'settings') page = <Settings user={auth?.user} onUserChange={loadAuth} />;
  else if (section === 'help') page = <Help />;
  else page = <div className="empty"><h1>Страница не найдена</h1><a href="#/overview">Вернуться к обзору</a></div>;
  return <Theme appearance={theme} accentColor="violet" grayColor="gray" radius="medium" scaling="100%">
    {!auth ? <div className="auth-wrap"><div className="auth-panel"><h1>MP</h1>{authError ? <><Notice error={authError} /><Button onClick={loadAuth}>Повторить подключение</Button></> : <Loading />}</div></div> : !auth.authenticated ? <Auth initialized={auth.initialized} setupTokenRequired={auth.setup_token_required} onAuth={onAuth} /> : <div className="app-shell">
      <a className="skip-link" href="#main-content" onClick={e => { e.preventDefault(); document.getElementById('main-content')?.focus(); }}>Перейти к содержимому</a>
      <aside className="sidebar"><a href="#/overview" className="brand"><span className="brand-mark">MP</span><span>Minecraft Panel<small>Ваши серверы</small></span></a>
        <nav aria-label="Основная навигация">{nav.filter(item => !operator || item.path !== 'nodes').map(item => <a key={item.path} href={`#/${item.path}`} aria-current={section === item.path ? 'page' : undefined}><item.icon />{item.label}</a>)}</nav>
        <div className="sidebar-bottom"><div className="account"><Avatar size="2" fallback={auth.user?.username.slice(0, 1).toUpperCase() || 'U'} /><div><strong>{auth.user?.username}</strong><small>{({ owner: 'Владелец', admin: 'Администратор', operator: 'Оператор', viewer: 'Наблюдатель' } as Record<string, string>)[auth.user?.role || 'viewer']}</small></div></div><div className="actions"><Button variant="ghost" color="gray" onClick={() => setTheme(theme === 'light' ? 'dark' : 'light')}>{theme === 'light' ? <MoonIcon /> : <SunIcon />}{theme === 'light' ? 'Темная' : 'Светлая'}</Button><Button variant="ghost" color="gray" disabled={action.busy} onClick={() => allowNavigation() && action.run(async () => { await api('/auth/logout', 'POST'); rememberAuth({ initialized: true, authenticated: false }); setAuth({ initialized: true, authenticated: false }); })}><ExitIcon />Выйти</Button></div></div>
      </aside>
      <main id="main-content" tabIndex={-1}><div className="content"><Notice error={action.error} /><Suspense fallback={<Loading />}><div key={route}>{page}</div></Suspense></div></main>
    </div>}
  </Theme>;
}
