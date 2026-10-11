import { useEffect, useRef, useState } from 'react';
import { Button } from '@radix-ui/themes';
import { api, enc } from './api';
import { Confirm, date, Input, Loading, Notice, Refresh, Status, Unavailable, useAction, useResource, useUnsavedChanges } from './ui';
type Route = { server_id: string; domain: string; backend: string; status: string; error?: string; updated_at?: string };
type RouteState = { enabled: boolean; provider?: string; base_domain?: string; default_domain?: string; route?: Route };
export const gatewayName = (provider?: string) => provider === 'mc-router' ? 'mc-router' : provider === 'gate' ? 'Gate Lite' : 'Игровой шлюз';
export function Network({ id, endpoint, canManage }: { id: string; endpoint?: string; canManage: boolean }) {
  const path = `/servers/${enc(id)}/route`;
  const resource = useResource<RouteState>(path, 10000);
  const [draft, setDraft] = useState({ domain: '', backend: '' });
  const [saved, setSaved] = useState(draft);
  const action = useAction();
  const dirty = draft.domain !== saved.domain || draft.backend !== saved.backend;
  const dirtyRef = useRef(false); dirtyRef.current = dirty;
  useUnsavedChanges(canManage && dirty);
  useEffect(() => {
    if (!resource.data || dirtyRef.current) return;
    const next = { domain: resource.data.route?.domain || resource.data.default_domain || '', backend: resource.data.route?.backend || '' };
    setDraft(next); setSaved(next);
  }, [resource.data?.route?.domain, resource.data?.route?.backend, resource.data?.default_domain]);
  const route = resource.data?.route;
  return <section><div className="section-title"><h2>Игровой адрес</h2><Refresh onClick={resource.refresh} /></div><Notice error={resource.error || action.error || route?.error} success={action.success} />
    <dl className="review-list"><div><dt>Прямое подключение</dt><dd className="mono">{endpoint || 'Адрес хоста недоступен'}</dd></div></dl>
    {resource.loading ? <Loading /> : !resource.data ? <Unavailable onRetry={resource.refresh} /> : !resource.data.enabled ? <Notice>Игровой шлюз не настроен на сервере панели. Сейчас используйте адрес хоста и игровой порт. Для игровых доменов подключите mc-router или Gate Lite в конфигурации панели. <a href="#/help">Подробнее о сети</a></Notice> : <>
      <p className="prose">{gatewayName(resource.data.provider)} пересылает соединения для этого домена на выбранный Minecraft Java сервер. DNS должен указывать на шлюз, а игровой хост должен быть доступен со шлюза.</p>
      {route ? <dl className="review-list"><div><dt>Домен</dt><dd className="mono">{route.domain}</dd></div><div><dt>Адрес за шлюзом</dt><dd className="mono">{route.backend}</dd></div><div><dt>Правило шлюза</dt><dd><Status value={route.status} /></dd></div><div><dt>Обновлено</dt><dd>{date(route.updated_at)}</dd></div></dl> : <Notice>Игровой домен еще не назначен. Предложенный домен появится на шлюзе после сохранения.</Notice>}
      <Notice>Статус правила подтверждает настройку шлюза. Для подключения также нужны DNS и доступный игровой сервер. Чтобы направить весь трафик через шлюз, ограничьте доступ к игровому порту хоста адресом шлюза.</Notice>
      {canManage && <form className="narrow-form" onSubmit={e => { e.preventDefault(); void action.run(async () => {
        const result = await api<Route>(path, 'PUT', { domain: draft.domain.trim(), backend: draft.backend.trim() || undefined });
        const next = { domain: result.domain, backend: result.backend }; setDraft(next); setSaved(next); resource.refresh();
      }, 'Правило отправлено на шлюз. Статус обновится после применения.'); }}><Input label="Игровой домен" value={draft.domain} onChange={e => setDraft({ ...draft, domain: e.target.value })} required maxLength={253} disabled={action.busy || route?.status === 'removing'} autoComplete="off" spellCheck={false} placeholder={resource.data.base_domain ? `survival.${resource.data.base_domain}` : 'survival.example.com'} hint={resource.data.default_domain ? `Предложение: ${resource.data.default_domain}. DNS настраивается отдельно.` : 'Укажите домен, который будет направлен на игровой шлюз.'} /><details><summary>Адрес сервера за шлюзом</summary><Input label="Адрес сервера, IP или имя:порт" value={draft.backend} onChange={e => setDraft({ ...draft, backend: e.target.value })} disabled={action.busy || route?.status === 'removing'} autoComplete="off" spellCheck={false} placeholder={endpoint} hint="Оставьте пустым для адреса хоста и порта этого сервера. Укажите адрес, доступный со шлюза." /></details><div className="actions form-actions"><Button type="submit" loading={action.busy} disabled={!draft.domain.trim() || !!route && !dirty || route?.status === 'removing'}>{route ? 'Сохранить правило' : 'Назначить домен'}</Button>{route && <Confirm danger disabled={action.busy || route.status === 'removing' || dirty} title="Удалить игровой домен?" description="Шлюз перестанет направлять новые соединения для этого домена. Minecraft на хосте продолжит работать." onConfirm={() => action.run(async () => { await api(path, 'DELETE'); resource.refresh(); }, 'Удаление правила запрошено.')}>Удалить правило</Confirm>}</div>{dirty && <small className="subline">Есть несохраненные изменения.</small>}</form>}
    </>}
  </section>;
}
