import { useEffect, useRef, useState } from 'react';
import { Avatar, Badge, Button, IconButton, TextField } from '@radix-ui/themes';
import { Cross2Icon, DownloadIcon, ExternalLinkIcon, MagnifyingGlassIcon, ReaderIcon } from '@radix-ui/react-icons';
import { api, APIError, enc } from './api';
import type { Job, ModrinthPlan, ModrinthProject, ModrinthSearch, ModrinthVersion, Node, Server, User } from './api';
import { bytes, Choice, date, Empty, Loading, message, Notice, PageTitle, Refresh, Status, useAction, useResource } from './ui';

const supported = (server: Server) => ['fabric', 'forge', 'neoforge', 'paper', 'spigot'].includes(server.core);
const catalogAgent = (node?: Node) => { const match = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(node?.agent_version || ''); return !!match && (Number(match[1]) > 0 || Number(match[2]) >= 4); };
const count = (value: number) => value.toLocaleString('ru-RU');
const projectLink = (project: ModrinthProject) => `https://modrinth.com/project/${enc(project.slug || project.id)}`;

export function Modrinth({ serverID, user }: { serverID?: string; user?: User }) {
  const servers = useResource<Server[]>('/servers', 5000);
  const available = servers.data?.filter(supported) || [];
  const requested = servers.data?.find(server => server.id === serverID);
  const selected = serverID ? requested : available[0];
  return <div className="modrinth-catalog"><PageTitle title="Каталог" description="Моды и плагины из Modrinth для версии и ядра вашего сервера." />
    <Notice error={servers.error} />
    {servers.loading ? <Loading /> : servers.data === undefined ? <Refresh onClick={servers.refresh} /> : <>
      {available.length > 0 && <div className="catalog-server"><Choice label="Установить на сервер" value={selected && supported(selected) ? selected.id : ''} onChange={id => { location.hash = `/catalog/${enc(id)}`; }} items={available.map(server => ({ value: server.id, label: `${server.name} · ${server.core} ${server.minecraft_version}` }))} />{selected && <a href={`#/servers/${enc(selected.id)}`}>Открыть сервер</a>}</div>}
      {serverID && !requested ? <Empty title="Сервер недоступен">Выберите сервер из списка. Возможно, он удалён или у вас нет доступа к нему.</Empty> : selected && supported(selected) ? <CatalogContent key={`${selected.id}/${selected.core}/${selected.minecraft_version}`} server={selected} user={user} /> : <Empty title="Нужно ядро с поддержкой модов или плагинов">Каталог работает с Fabric, Forge, NeoForge, Paper и Spigot. Vanilla не загружает моды и плагины. Выберите другое ядро в настройках сервера.{!servers.data.length && <><br /><a href="#/servers">Перейти к серверам</a></>}</Empty>}
    </>}
  </div>;
}

function CatalogContent({ server, user }: { server: Server; user?: User }) {
  const base = `/servers/${enc(server.id)}/modrinth`;
  const nodes = useResource<Node[]>('/nodes', 10000);
  const jobs = useResource<Job[]>('/jobs', 4000);
  const node = nodes.data?.find(item => item.id === server.node_id);
  const active = jobs.data?.find(job => job.server_id === server.id && ['queued', 'running'].includes(job.status));
  const canManage = ['owner', 'admin', 'operator'].includes(user?.role || 'viewer');
  const canWrite = canManage && catalogAgent(node) && node?.status === 'online' && ['stopped', 'error'].includes(server.status) && jobs.data !== undefined && !jobs.error && !active;
  const [draft, setDraft] = useState('');
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState('downloads');
  const [offset, setOffset] = useState(0);
  const [selected, setSelected] = useState<ModrinthProject>();
  const search = useResource<ModrinthSearch>(`${base}/search?q=${enc(query)}&sort=${enc(sort)}&offset=${offset}`);
  return <>
    <div className="catalog-context"><span>Совместимость: <strong>{server.core} · Minecraft {server.minecraft_version}</strong></span><Status value={server.status} /></div>
    <form className="catalog-search" onSubmit={event => { event.preventDefault(); setQuery(draft.trim()); setOffset(0); if (draft.trim() && sort === 'downloads') setSort('relevance'); else search.refresh(); }}>
      <div className="field"><label htmlFor="modrinth-search">Найти мод или плагин</label><TextField.Root id="modrinth-search" type="search" size="3" value={draft} onChange={event => setDraft(event.target.value)} placeholder="Название или описание" maxLength={200}><TextField.Slot><MagnifyingGlassIcon /></TextField.Slot></TextField.Root></div>
      <Button type="submit" size="3">Найти</Button>
      <Choice label="Порядок" value={sort} onChange={value => { setSort(value); setOffset(0); }} items={[{ value: 'downloads', label: 'Популярные' }, { value: 'relevance', label: 'По совпадению' }, { value: 'updated', label: 'Недавно обновлённые' }]} />
    </form>
    <div className={`catalog-layout${selected ? ' has-detail' : ''}`}>
      <section className="catalog-results" aria-label="Проекты Modrinth">
        <div className="section-title"><h2>{query ? `Результаты: ${query}` : sort === 'updated' ? 'Недавно обновлённые' : 'Популярные из Modrinth'}</h2>{search.data && <small>{count(search.data.total_hits)} проектов</small>}</div>
        <Notice error={search.error} />
        {search.loading ? <Loading /> : search.data === undefined ? <Refresh onClick={search.refresh} /> : search.data.hits.length ? <>
          <ul className="catalog-projects">{search.data.hits.map(project => <li key={project.id}><button type="button" className="catalog-project" id={`modrinth-${project.id}`} aria-pressed={selected?.id === project.id} onClick={() => setSelected(project)}><Avatar src={project.icon_url || undefined} fallback={<ReaderIcon />} size="3" radius="medium" /><span className="catalog-project-text"><strong>{project.title}</strong><small>Автор: {project.author || 'Не указан'}</small><span className="catalog-description">{project.description}</span><small className="catalog-downloads"><DownloadIcon />{count(project.downloads)} скачиваний</small></span></button></li>)}</ul>
          <div className="catalog-pagination"><Button variant="soft" color="gray" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - search.data!.limit))}>Назад</Button><small>{count(offset + 1)}–{count(offset + search.data.hits.length)} из {count(search.data.total_hits)}</small><Button variant="soft" color="gray" disabled={offset + search.data.limit >= search.data.total_hits || offset + search.data.limit > 10000} onClick={() => setOffset(offset + search.data!.limit)}>Далее</Button></div>
        </> : <Empty title="Совместимых проектов не найдено">Попробуйте другое название. Каталог показывает только проекты для {server.core} и Minecraft {server.minecraft_version}.</Empty>}
      </section>
      {selected && <ProjectDetail key={selected.id} project={selected} server={server} canWrite={canWrite} canManage={canManage} nodeOnline={node?.status === 'online'} agentReady={catalogAgent(node)} active={active} onClose={() => { document.getElementById(`modrinth-${selected.id}`)?.focus(); setSelected(undefined); }} onInstalled={() => jobs.refresh()} />}
    </div>
  </>;
}

function ProjectDetail({ project, server, canWrite, canManage, nodeOnline, agentReady, active, onClose, onInstalled }: { project: ModrinthProject; server: Server; canWrite: boolean; canManage: boolean; nodeOnline: boolean; agentReady: boolean; active?: Job; onClose: () => void; onInstalled: () => void }) {
  const base = `/servers/${enc(server.id)}/modrinth`;
  const versions = useResource<ModrinthVersion[]>(`${base}/projects/${enc(project.id)}/versions`);
  const [choice, setChoice] = useState('');
  const versionID = choice || versions.data?.find(version => version.version_type === 'release')?.id || versions.data?.[0]?.id || '';
  const [plan, setPlan] = useState<ModrinthPlan>();
  const [planError, setPlanError] = useState('');
  const [planning, setPlanning] = useState(false);
  const [previewRevision, setPreviewRevision] = useState(0);
  const [lastJob, setLastJob] = useState<Job>();
  const job = useResource<Job>(lastJob ? `/jobs/${enc(lastJob.id)}` : null, 3000);
  const action = useAction();
  const detail = useRef<HTMLElement>(null);
  const knownJob = job.data || lastJob;
  const pending = knownJob && ['queued', 'running'].includes(knownJob.status);
  useEffect(() => { detail.current?.focus({ preventScroll: true }); if (window.matchMedia('(max-width: 1050px)').matches) detail.current?.scrollIntoView({ block: 'start' }); }, []);
  useEffect(() => {
    setPlan(undefined); setPlanError('');
    if (!versionID) { setPlanning(false); return; }
    const controller = new AbortController(); setPlanning(true);
    api<ModrinthPlan>(`${base}/plan`, 'POST', { project_id: project.id, version_id: versionID }, controller.signal).then(result => { if (!controller.signal.aborted) setPlan(result); }).catch(error => { if (!controller.signal.aborted) setPlanError(message(error)); }).finally(() => { if (!controller.signal.aborted) setPlanning(false); });
    return () => controller.abort();
  }, [base, project.id, versionID, previewRevision]);
  useEffect(() => { if (job.data && ['succeeded', 'failed'].includes(job.data.status)) onInstalled(); }, [job.data?.status]);
  const reason = !canManage ? 'Вашей роли доступен просмотр каталога.' : !nodeOnline ? 'Подключите игровой хост, чтобы установить файлы.' : !agentReady ? 'Обновите агент игрового хоста до v0.4.0 или новее для установки из каталога.' : !['stopped', 'error'].includes(server.status) ? 'Остановите сервер перед установкой.' : active || pending ? 'Дождитесь завершения текущего задания.' : !canWrite ? 'Получаем актуальное состояние заданий.' : '';
  return <aside className="catalog-detail" ref={detail} tabIndex={-1} aria-label={`Установка ${project.title}`}>
    <div className="section-title"><h2>{project.title}</h2><IconButton variant="ghost" color="gray" aria-label="Закрыть проект" onClick={onClose}><Cross2Icon /></IconButton></div>
    <p className="catalog-detail-description">{project.description}</p><a className="catalog-source" href={projectLink(project)} target="_blank" rel="noopener noreferrer">Страница на Modrinth <ExternalLinkIcon /></a>
    <Notice error={versions.error || planError || action.error || job.error} />
    {versions.loading ? <Loading /> : versions.data === undefined ? <Refresh onClick={versions.refresh} /> : !versions.data.length ? <Notice>Совместимых версий пока нет. Возможно, проект ещё не обновлён для этого сервера.</Notice> : <>
      <Choice label="Версия проекта" value={versionID} disabled={action.busy || !!pending} onChange={value => { setChoice(value); action.clear(); }} items={versions.data.map(version => ({ value: version.id, label: `${version.version_number} · ${version.version_type === 'release' ? 'релиз' : version.version_type === 'beta' ? 'бета' : 'альфа'} · ${date(version.date_published).split(',')[0]}` }))} />
      {planning ? <Loading /> : plan ? <><h3>Будет установлено</h3><ul className="catalog-plan">{plan.files.map(file => <li key={`${file.project_id}/${file.name}`}><div><strong>{file.title}</strong>{file.required && <Badge color="gray" size="1">Зависимость</Badge>}</div><code>{file.name}</code><small>{bytes(file.size)}</small></li>)}</ul>{plan.warnings?.map((warning, index) => <Notice key={index}>{warning}</Notice>)}</> : planError ? <Button variant="soft" color="gray" onClick={() => setPreviewRevision(value => value + 1)}>Повторить проверку файлов</Button> : null}
      <div className="catalog-install"><Button disabled={!canWrite || !plan?.plan_hash || plan.version_id !== versionID || planning || !!pending} loading={action.busy} onClick={() => action.run(async () => { try { const result = await api<Job>(`${base}/install`, 'POST', { project_id: project.id, version_id: versionID, plan_hash: plan!.plan_hash }); setLastJob(result); onInstalled(); } catch (error) { if (error instanceof APIError && error.status === 409) { setPlan(undefined); setPreviewRevision(value => value + 1); } throw error; } })}><DownloadIcon />Установить{plan && plan.files.length > 1 ? ` (${plan.files.length})` : ''}</Button><small>{reason || 'Файлы попадут в каталог модов или плагинов. Затем запустите сервер, чтобы применить изменения.'}</small></div>
    </>}
    {knownJob && <div className="catalog-job"><Status value={knownJob.status} /><p>{knownJob.status === 'succeeded' ? 'Установка завершена. Запустите сервер для применения.' : knownJob.error || knownJob.step || 'Задание отправлено на игровой хост.'}</p><a href={`#/jobs/${enc(knownJob.id)}`}>Открыть журнал установки</a></div>}
  </aside>;
}
