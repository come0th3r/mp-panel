import { createContext, useContext, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Button, IconButton, Tooltip } from '@radix-ui/themes';
import { CheckIcon, CopyIcon, Cross2Icon, ExternalLinkIcon, ReloadIcon, UpdateIcon } from '@radix-ui/react-icons';
import { api } from './api';
import type { User } from './api';
import { date, Loading, message, Notice, useAction } from './ui';

type UpdateStatus = { current_version: string; latest_version?: string; update_available: boolean; release_url?: string; published_at?: string; checked_at?: string; error?: string };
type UpdateContext = { data?: UpdateStatus; loading: boolean; checking: boolean; error: string; check: () => Promise<void>; user?: User };
const Updates = createContext<UpdateContext>({ loading: false, checking: false, error: '', check: async () => {} });
const REPOSITORY = 'https://github.com/come0th3r/mp-panel';

export function UpdatesProvider({ user, children }: { user?: User; children: ReactNode }) {
  const owner = user?.role === 'owner';
  const [data, setData] = useState<UpdateStatus>();
  const [loading, setLoading] = useState(owner);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState('');
  const lock = useRef(false);
  useEffect(() => {
    if (!owner) { setData(undefined); setLoading(false); setError(''); return; }
    const controller = new AbortController();
    let first = true;
    let initialCheck: number | undefined;
    const load = async () => {
      try { const result = await api<UpdateStatus>('/updates', 'GET', undefined, controller.signal); if (!controller.signal.aborted) { setData(result); setError(''); if (first && !result.checked_at && !result.error) initialCheck = window.setTimeout(() => void load(), 5000); } }
      catch (cause) { if (!controller.signal.aborted) setError(message(cause)); }
      finally { first = false; if (!controller.signal.aborted) setLoading(false); }
    };
    void load();
    const timer = window.setInterval(() => void load(), 30 * 60 * 1000);
    return () => { controller.abort(); clearInterval(timer); if (initialCheck) clearTimeout(initialCheck); };
  }, [owner, user?.id]);
  const check = async () => {
    if (!owner || lock.current) return;
    lock.current = true; setChecking(true); setError('');
    try { setData(await api<UpdateStatus>('/updates/check', 'POST')); }
    catch (cause) { setError(message(cause)); }
    finally { lock.current = false; setChecking(false); }
  };
  return <Updates.Provider value={{ data, loading, checking, error, check, user }}>{children}</Updates.Provider>;
}

function dismissed(key: string) { try { return localStorage.getItem(key) === '1'; } catch { return false; } }

export function UpdateIndicator() {
  const { data, user } = useContext(Updates);
  const [hidden, setHidden] = useState('');
  if (!data?.update_available || !data.latest_version || user?.role !== 'owner') return null;
  const key = `mp-update-dismissed:${user.id}:${data.latest_version}`;
  if (hidden === key || dismissed(key)) return null;
  return <div className="update-indicator"><a href="#/settings/updates"><UpdateIcon /><span>Доступна {data.latest_version}</span></a><Tooltip content="Не напоминать об этой версии"><IconButton size="1" color="gray" variant="ghost" aria-label="Не напоминать об этой версии" onClick={() => { setHidden(key); try { localStorage.setItem(key, '1'); } catch { /* The current visit can still dismiss the indicator. */ } }}><Cross2Icon /></IconButton></Tooltip></div>;
}

export function UpdateSettings() {
  const { data, loading, checking, error, check } = useContext(Updates);
  const copy = useAction();
  const tag = data?.latest_version && /^v?\d+\.\d+\.\d+(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(data.latest_version) ? data.latest_version : undefined;
  const release = tag ? `${REPOSITORY}/releases/tag/${encodeURIComponent(tag)}` : `${REPOSITORY}/releases/latest`;
  const command = tag ? `curl -fL ${REPOSITORY}/releases/download/${encodeURIComponent(tag)}/setup.sh -o setup.sh &&\nsudo bash setup.sh` : '';
  return <section className="panel-updates"><div className="section-title"><h2>Обновления панели</h2><Button variant="soft" color="gray" loading={checking} onClick={() => void check()}><ReloadIcon />Проверить сейчас</Button></div><p className="prose">Панель проверяет стабильные релизы на GitHub. Установка выполняется через SSH на сервере панели.</p><Notice error={error || data?.error || copy.error} success={copy.success} />
    {loading ? <Loading /> : <><dl className="review-list"><div><dt>Установленная версия</dt><dd className="mono">{data?.current_version || 'Не определена'}</dd></div><div><dt>Последний стабильный релиз</dt><dd className="mono">{data?.latest_version || 'Нет данных'}</dd></div>{data?.checked_at && <div><dt>Последняя проверка</dt><dd>{date(data.checked_at)}</dd></div>}{data?.published_at && <div><dt>Релиз опубликован</dt><dd>{date(data.published_at)}</dd></div>}</dl>
      {data?.latest_version && !data.error && <p className="update-state">{data.update_available ? <UpdateIcon /> : <CheckIcon />}<span>{data.update_available ? `Можно обновиться до ${data.latest_version}.` : 'Установлена актуальная версия панели.'}</span></p>}
      <a className="update-release-link" href={release} target="_blank" rel="noreferrer">Изменения в релизе <ExternalLinkIcon /></a>
      {data?.update_available && command && <div className="update-instructions"><h3>Как обновить</h3><ol><li>Сохраните резервную копию PostgreSQL, тома <code>panel-data</code> и файла <code>.env</code>. Копии игровых серверов храните отдельно.</li><li>Подключитесь по SSH к серверу панели и выполните команды ниже.</li><li>Если панель установлена не в <code>/opt/mp-panel</code>, добавьте к запуску установщика <code>--directory /путь/к/панели</code>. Существующий <code>.env</code> сохраняется.</li><li>После обновления панели обновите агенты через «Хосты → Обновить агент», чтобы новые функции были доступны на игровых хостах.</li></ol><div className="update-command"><pre aria-label="Команды обновления панели">{command}</pre><Tooltip content="Скопировать команды"><IconButton color="gray" variant="soft" aria-label="Скопировать команды обновления" disabled={copy.busy} onClick={() => void copy.run(async () => { await navigator.clipboard.writeText(command); }, 'Команды скопированы.')}><CopyIcon /></IconButton></Tooltip></div><small>Во время обновления веб-панель может быть кратковременно недоступна. Установщик проверяет контрольную сумму архива, сохраняет копии базы и данных панели и пытается вернуть прежнюю версию при сбое.</small></div>}
    </>}
  </section>;
}
