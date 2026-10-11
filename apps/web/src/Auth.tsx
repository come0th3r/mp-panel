import { useState } from 'react';
import { Button } from '@radix-ui/themes';
import { LockClosedIcon } from '@radix-ui/react-icons';
import { api, APIError } from './api';
import type { AuthStatus } from './api';
import { Input, Notice, useAction } from './ui';
export function Auth({ initialized, setupTokenRequired, onAuth }: { initialized: boolean; setupTokenRequired?: boolean; onAuth: (status: AuthStatus) => void }) {
  const [username, setUsername] = useState(''); const [password, setPassword] = useState(''); const [code, setCode] = useState(''); const [totp, setTotp] = useState(false); const action = useAction();
  const [setupToken, setSetupToken] = useState('');
  return <div className="auth-wrap"><div className="auth-panel"><div className="brand-mark">MP</div><h1>{initialized ? 'Вход в панель' : 'Создайте владельца'}</h1><p>{initialized ? 'Управление Minecraft на ваших хостах.' : 'Первый аккаунт получает доступ к настройкам панели и подключению хостов.'}</p><Notice error={action.error} />
    <form onSubmit={e => { e.preventDefault(); void action.run(async () => { try { const status = await api<AuthStatus>(initialized ? '/auth/login' : '/auth/setup', 'POST', { username, password, ...(!initialized ? { setup_token: setupToken } : {}), ...(code ? { code } : {}) }); setPassword(''); setCode(''); setSetupToken(''); onAuth(status); } catch (err) { if (err instanceof APIError && err.code === 'totp_required') { setTotp(true); throw new Error('Введите код из приложения-аутентификатора или код восстановления.'); } throw err; } }); }}>
      {!initialized && setupTokenRequired && <Input label="Ключ первоначальной настройки" type="password" required autoComplete="off" value={setupToken} onChange={e => setSetupToken(e.target.value)} hint="Возьмите MP_SETUP_TOKEN или содержимое data/keys/setup.token на сервере панели." />}
      <Input label="Имя пользователя" autoComplete="username" required minLength={3} maxLength={64} value={username} onChange={e => setUsername(e.target.value)} />
      <Input label="Пароль" type="password" autoComplete={initialized ? 'current-password' : 'new-password'} required minLength={initialized ? undefined : 12} value={password} onChange={e => setPassword(e.target.value)} hint={!initialized ? 'Не менее 12 символов. После входа подключите двухфакторную защиту.' : undefined} />
      {totp && <Input label="Код 2FA или восстановления" autoComplete="one-time-code" value={code} onChange={e => setCode(e.target.value)} required autoFocus />}
      <Button size="3" type="submit" loading={action.busy} className="full"><LockClosedIcon />{initialized ? 'Войти' : 'Создать аккаунт'}</Button>
    </form>
  </div></div>;
}
