import { useRef, useState } from 'react';
import { Button, Table } from '@radix-ui/themes';
import { api, enc } from './api';
import { gatewayName } from './Network';
import { allowNavigation, Confirm, date, Empty, Input, Loading, Notice, Refresh, Status, Unavailable, useAction, useResource, useUnsavedChanges } from './ui';

type ExternalRoute = { server_id: string; name: string; external: boolean; domain: string; backend: string; status: string; error?: string; updated_at?: string };
type RouteList = { enabled: boolean; provider?: string; base_domain?: string; routes: ExternalRoute[] };
type Draft = { name: string; domain: string; backend: string };
const blank: Draft = { name: '', domain: '', backend: '' };

export function ExternalRoutes() {
  const resource = useResource<RouteList>('/external-routes', 10000);
  const action = useAction();
  const [editing, setEditing] = useState<string>();
  const [draft, setDraft] = useState<Draft>(blank);
  const [saved, setSaved] = useState<Draft>(blank);
  const formRef = useRef<HTMLFormElement>(null);
  const dirty = editing !== undefined && (draft.name !== saved.name || draft.domain !== saved.domain || draft.backend !== saved.backend);
  useUnsavedChanges(dirty);
  const current = resource.data?.routes.find(route => route.server_id === editing);
  const missing = !!editing && !!resource.data && !current;
  const removing = current?.status === 'removing';
  const editable = !!resource.data?.enabled && !missing && !removing;
  const begin = (route?: ExternalRoute) => {
    if (action.busy || !allowNavigation()) return;
    const next = route ? { name: route.name, domain: route.domain, backend: route.backend } : blank;
    setEditing(route?.server_id || ''); setDraft(next); setSaved(next); action.clear();
    requestAnimationFrame(() => { formRef.current?.scrollIntoView({ block: 'nearest' }); formRef.current?.querySelector<HTMLInputElement>('input')?.focus(); });
  };
  const close = () => { if (!action.busy && allowNavigation()) { setEditing(undefined); setDraft(blank); setSaved(blank); action.clear(); } };
  return <section>
    <div className="section-title"><h2>Внешние серверы</h2><div className="actions"><Refresh onClick={resource.refresh} />{editing === undefined && <Button disabled={!resource.data?.enabled || resource.loading} onClick={() => begin()}>Добавить внешний сервер</Button>}</div></div>
    <p className="prose">Назначьте игровой домен уже установленному Minecraft Java серверу. Достаточно адреса, доступного со шлюза; SSH и агент не нужны. Файлы, ядро и запуск остаются под управлением существующего сервера.</p>
    <Notice error={resource.error || action.error} success={action.success} />
    {resource.loading ? <Loading /> : !resource.data ? <Unavailable onRetry={resource.refresh} /> : <>
      {!resource.data.enabled ? <Notice>Игровой шлюз не настроен. Подключите mc-router или Gate Lite в конфигурации панели, чтобы назначать домены. <a href="#/help">Подробнее о сети</a></Notice> : <Notice>{gatewayName(resource.data.provider)} направляет соединения по домену. DNS должен указывать на шлюз. Статус «Правило применено» подтверждает маршрут, но не доступность игрового сервера.{resource.data.base_domain && <> Базовый домен: <code>{resource.data.base_domain}</code>.</>}</Notice>}
      {editing !== undefined && <form ref={formRef} className="narrow-form external-route-form" onSubmit={e => { e.preventDefault(); if (!editable) return; void action.run(async () => {
        await api<ExternalRoute>(editing ? `/external-routes/${enc(editing)}` : '/external-routes', editing ? 'PUT' : 'POST', { name: draft.name.trim(), domain: draft.domain.trim(), backend: draft.backend.trim() });
        setEditing(undefined); setDraft(blank); setSaved(blank); resource.refresh();
      }, 'Маршрут сохранен. Правило отправится на шлюз автоматически.'); }}>
        <h3>{editing ? 'Изменить внешний сервер' : 'Добавить внешний сервер'}</h3>
        {missing && <Notice error="Маршрут уже удален. Скопируйте нужные значения перед закрытием формы." />}
        {removing && <Notice>Маршрут удаляется. Дождитесь завершения или закройте форму.</Notice>}
        <Input label="Название сервера" value={draft.name} onChange={e => setDraft({ ...draft, name: e.target.value })} required maxLength={120} disabled={action.busy || !editable} autoComplete="off" placeholder="Выживание" />
        <Input label="Игровой домен" value={draft.domain} onChange={e => setDraft({ ...draft, domain: e.target.value })} required maxLength={253} disabled={action.busy || !editable} autoComplete="off" spellCheck={false} placeholder={resource.data.base_domain ? `survival.${resource.data.base_domain}` : 'survival.example.com'} hint="Полный домен без протокола и порта. DNS настраивается отдельно." />
        <Input label="Адрес сервера, IP или имя:порт" value={draft.backend} onChange={e => setDraft({ ...draft, backend: e.target.value })} required maxLength={260} disabled={action.busy || !editable} autoComplete="off" spellCheck={false} placeholder="192.168.1.200:25565" hint="Адрес должен быть доступен со шлюза. Для IPv6 используйте [адрес]:порт." />
        <div className="actions form-actions"><Button type="submit" loading={action.busy} disabled={!editable || !draft.name.trim() || !draft.domain.trim() || !draft.backend.trim() || !!editing && !dirty}>{editing ? 'Сохранить маршрут' : 'Добавить маршрут'}</Button><Button type="button" color="gray" variant="soft" disabled={action.busy} onClick={close}>Отмена</Button></div>
        {dirty && <small className="subline">Есть несохраненные изменения.</small>}
      </form>}
      {resource.data.routes.length ? <div className="table-wrap external-route-list"><Table.Root><Table.Header><Table.Row><Table.ColumnHeaderCell>Сервер и домен</Table.ColumnHeaderCell><Table.ColumnHeaderCell>Адрес за шлюзом</Table.ColumnHeaderCell><Table.ColumnHeaderCell>Правило шлюза</Table.ColumnHeaderCell><Table.ColumnHeaderCell>Действия</Table.ColumnHeaderCell></Table.Row></Table.Header><Table.Body>{resource.data.routes.map(route => <Table.Row key={route.server_id}>
        <Table.RowHeaderCell>{route.name}<span className="subline mono">{route.domain}</span></Table.RowHeaderCell>
        <Table.Cell className="mono">{route.backend}</Table.Cell>
        <Table.Cell><Status value={route.status} /><small className="subline">{date(route.updated_at)}</small>{route.error && <Notice error={route.error} />}</Table.Cell>
        <Table.Cell><div className="actions"><Button variant="soft" color="gray" aria-label={`Изменить ${route.name}`} disabled={action.busy || !resource.data?.enabled || route.status === 'removing'} onClick={() => begin(route)}>Изменить</Button><Confirm danger title={`Удалить маршрут ${route.domain}?`} description="Шлюз перестанет направлять новые соединения для этого домена. Существующий Minecraft сервер продолжит работать, его файлы сохранятся." disabled={action.busy || dirty || route.status === 'removing'} onConfirm={() => action.run(async () => { await api(`/external-routes/${enc(route.server_id)}`, 'DELETE'); if (editing === route.server_id) { setEditing(undefined); setDraft(blank); setSaved(blank); } resource.refresh(); }, 'Удаление маршрута запрошено.')}>Удалить</Confirm></div></Table.Cell>
      </Table.Row>)}</Table.Body></Table.Root></div> : editing === undefined && <Empty title="Внешние серверы еще не добавлены">Добавьте IP и порт существующего сервера, чтобы подключаться к нему через игровой домен.</Empty>}
    </>}
  </section>;
}
