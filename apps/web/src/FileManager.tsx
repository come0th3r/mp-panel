import { useEffect, useRef, useState } from 'react';
import type { DragEvent, KeyboardEvent, ReactNode } from 'react';
import { Button, Checkbox, IconButton, Table, TextArea, Tooltip } from '@radix-ui/themes';
import { ArchiveIcon, ChevronRightIcon, ClipboardIcon, CopyIcon, Cross2Icon, FileIcon, MoveIcon, Pencil1Icon, PlusIcon, ScissorsIcon, TrashIcon, UploadIcon } from '@radix-ui/react-icons';
import { api, enc, upload } from './api';
import type { FileEntry, Job, Server } from './api';
import { bytes, Confirm, date, Empty, Input, jobName, Loading, message, Notice, Refresh, Unavailable, useAction, useResource, useUnsavedChanges } from './ui';

type FileContent = { path: string; content: string; revision: string };
type Clipboard = { paths: string[]; mode: 'copy' | 'move' };
type UploadItem = { file: File; name: string };
type Form = 'directory' | 'upload' | 'move' | 'extract' | 'rename' | null;
const MAX_FILES = 100;
const MAX_BYTES = 256 * 1024 * 1024;
const DRAG_TYPE = 'application/x-mp-files';
const join = (directory: string, name: string) => directory ? `${directory}/${name}` : name;
const fileName = (path: string) => path.split('/').pop() || path;
const quantity = (count: number, one: string, few: string, many: string) => count % 100 >= 11 && count % 100 <= 14 ? many : count % 10 === 1 ? one : count % 10 >= 2 && count % 10 <= 4 ? few : many;
const childName = (value: string) => { const name = value.trim(); if (!name || name === '.' || name === '..' || /[\/\\]/.test(name)) throw new Error('Укажите имя без разделителей пути.'); return name; };

function FileTool({ label, children, disabled, onClick, accent = false }: { label: string; children: ReactNode; disabled?: boolean; onClick: () => void; accent?: boolean }) {
  return <Tooltip content={label}><span className="file-tool-hint" tabIndex={disabled ? 0 : undefined} aria-label={disabled ? label : undefined}><IconButton size="2" variant="soft" color={accent ? 'violet' : 'gray'} className="file-tool" aria-label={label} disabled={disabled} onClick={onClick}>{children}</IconButton></span></Tooltip>;
}

async function droppedFiles(items: DataTransferItemList, fallback: FileList): Promise<UploadItem[]> {
  const result: UploadItem[] = [];
  const add = (file: File, name: string) => {
    if (result.length >= MAX_FILES) throw new Error(`Можно загрузить до ${MAX_FILES} файлов за один раз. Разделите загрузку на несколько частей.`);
    result.push({ file, name });
  };
  const walk = async (entry: FileSystemEntry, parent = ''): Promise<void> => {
    const name = join(parent, entry.name);
    if (entry.isFile) {
      const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject));
      add(file, name);
    } else if (entry.isDirectory) {
      const reader = (entry as FileSystemDirectoryEntry).createReader();
      for (;;) {
        const children = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
        if (!children.length) break;
        for (const child of children) await walk(child, name);
      }
    }
  };
  const entries = Array.from(items).filter(item => item.kind === 'file').map(item => item.webkitGetAsEntry?.());
  if (entries.length && entries.every(Boolean)) {
    for (const entry of entries) await walk(entry!);
  } else {
    for (const file of Array.from(fallback)) add(file, file.name);
  }
  return result;
}

export function FileManager({ server, canWrite, onJob }: { server: Server; canWrite: boolean; onJob: (job: Job) => void }) {
  const base = `/servers/${enc(server.id)}`;
  const [path, setPath] = useState('');
  const files = useResource<FileEntry[]>(`${base}/files?path=${enc(path)}`);
  const [opened, setOpened] = useState<FileContent>();
  const [draft, setDraft] = useState('');
  const [selection, setSelection] = useState<string[]>([]);
  const [clipboard, setClipboard] = useState<Clipboard>();
  const [form, setForm] = useState<Form>(null);
  const [name, setName] = useState('');
  const [renamePath, setRenamePath] = useState('');
  const [destination, setDestination] = useState('');
  const [uploads, setUploads] = useState<UploadItem[]>([]);
  const [progress, setProgress] = useState<{ index: number; total: number; name: string; loaded: number; size: number }>();
  const [feedback, setFeedback] = useState({ error: '', success: '' });
  const [pending, setPending] = useState<{ id: string; mode?: 'copy' | 'move' }>();
  const job = useResource<Job>(pending ? `/jobs/${enc(pending.id)}` : null, 1500);
  const action = useAction();
  const picker = useRef<HTMLInputElement>(null);
  const folderPicker = useRef<HTMLInputElement>(null);
  const anchor = useRef<string | undefined>(undefined);
  const dragDepth = useRef(0);
  const [dragging, setDragging] = useState(false);
  const [dropFolder, setDropFolder] = useState('');
  const [readingDrop, setReadingDrop] = useState(false);
  const dirty = !!opened && draft !== opened.content;
  const busy = action.busy || !!pending || readingDrop;
  const writable = canWrite && !busy;
  const entries = files.data || [];
  const selectedEntry = selection.length === 1 ? entries.find(entry => entry.path === selection[0]) : undefined;
  const archive = selectedEntry && !selectedEntry.is_dir && /\.(zip|rar)$/i.test(selectedEntry.name) ? selectedEntry : undefined;
  const allSelected = !!entries.length && selection.length === entries.length;
  useUnsavedChanges(dirty);

  useEffect(() => {
    if (!pending || !job.data || !['succeeded', 'failed'].includes(job.data.status)) return;
    if (job.data.status === 'succeeded') {
      setFeedback({ error: '', success: `${jobName(job.data.type)}: завершено.` });
      setSelection([]);
      if (pending.mode === 'move') setClipboard(undefined);
    } else setFeedback({ error: job.data.error || 'Операция завершилась с ошибкой. Подробности доступны в журнале задания.', success: '' });
    setPending(undefined);
    files.refresh();
  }, [job.data, pending, files.refresh]);

  useEffect(() => {
    if (!files.data) return;
    setSelection(current => current.filter(item => files.data!.some(entry => entry.path === item)));
  }, [files.data]);

  const canLeave = () => !dirty || window.confirm('Несохраненные изменения будут потеряны. Продолжить?');
  const navigate = (target: string) => {
    if (action.busy || readingDrop || !canLeave()) return;
    setPath(target); setOpened(undefined); setSelection([]); setForm(null); setUploads([]); anchor.current = undefined;
  };
  const open = (entry: FileEntry) => {
    if (busy || !canLeave()) return;
    if (entry.is_dir) navigate(entry.path);
    else void action.run(async () => {
      const content = await api<FileContent>(`${base}/files/content?path=${enc(entry.path)}`);
      setOpened(content); setDraft(content.content); setForm(null);
    });
  };
  const queue = (result: Job, mode?: 'copy' | 'move') => {
    setPending({ id: result.id, mode }); setFeedback({ error: '', success: '' }); setForm(null); onJob(result);
  };
  const transfer = (paths: string[], target: string, mode: 'copy' | 'move') => {
    if (!writable || !paths.length) return;
    void action.run(async () => {
      const result = await api<Job>(`${base}/files/transfer`, 'POST', { paths, destination: target, mode });
      queue(result, mode);
    });
  };
  const copy = (mode: 'copy' | 'move') => {
    if (!selection.length || busy || (mode === 'move' && !writable)) return;
    action.clear(); setClipboard({ paths: [...selection], mode });
    setFeedback({ error: '', success: mode === 'copy' ? 'Скопировано в буфер. Откройте нужный каталог и нажмите «Вставить».' : 'Готово к перемещению. Откройте нужный каталог и нажмите «Вставить».' });
  };
  const toggle = (entry: FileEntry, checked: boolean, shift = false) => {
    let affected = [entry.path];
    if (shift && anchor.current) {
      const first = entries.findIndex(item => item.path === anchor.current);
      const last = entries.findIndex(item => item.path === entry.path);
      if (first >= 0 && last >= 0) affected = entries.slice(Math.min(first, last), Math.max(first, last) + 1).map(item => item.path);
    }
    setSelection(current => {
      const next = checked ? [...new Set([...current, ...affected])] : current.filter(item => !affected.includes(item));
      if (next.length > MAX_FILES) { setFeedback({ error: `Можно выделить до ${MAX_FILES} объектов за один раз.`, success: '' }); return current; }
      return next;
    });
    anchor.current = entry.path;
  };
  const selectAll = () => {
    if (allSelected) setSelection([]);
    else if (entries.length > MAX_FILES) setFeedback({ error: `В каталоге больше ${MAX_FILES} объектов. Выберите нужные по отдельности.`, success: '' });
    else setSelection(entries.map(entry => entry.path));
  };
  const receiveUploads = (items: UploadItem[]) => {
    if (!writable) return;
    if (!items.length) { setFeedback({ error: 'В выбранном каталоге нет файлов для загрузки. Пустые каталоги можно создать кнопкой «Новый каталог».', success: '' }); return; }
    if (items.length > MAX_FILES) { setFeedback({ error: `Можно загрузить до ${MAX_FILES} файлов за один раз.`, success: '' }); return; }
    if (items.some(item => item.file.size > MAX_BYTES)) { setFeedback({ error: 'Размер каждого файла не должен превышать 256 МиБ.', success: '' }); return; }
    if (new Set(items.map(item => item.name)).size !== items.length) { setFeedback({ error: 'В загрузке есть файлы с одинаковым именем. Измените имена или загрузите их в разные каталоги.', success: '' }); return; }
    setUploads(items); setForm('upload'); setFeedback({ error: '', success: '' });
  };
  const sendUploads = () => {
    if (!writable || !uploads.length) return;
    const target = path;
    const batch = [...uploads];
    void action.run(async () => {
      try {
        const artifacts: { path: string; upload_id: string }[] = [];
        for (const [index, item] of batch.entries()) {
          setProgress({ index: index + 1, total: batch.length, name: item.name, loaded: 0, size: item.file.size });
          const artifact = await upload(item.file, loaded => setProgress({ index: index + 1, total: batch.length, name: item.name, loaded, size: item.file.size }));
          artifacts.push({ path: join(target, item.name), upload_id: artifact.id });
        }
        const result = await api<Job>(`${base}/files/upload-batch`, 'POST', { files: artifacts });
        setUploads([]); queue(result);
      } finally { setProgress(undefined); }
    });
  };
  const drop = async (event: DragEvent<HTMLElement>, target = path) => {
    event.preventDefault(); event.stopPropagation(); dragDepth.current = 0; setDragging(false); setDropFolder('');
    if (!writable) return;
    if (event.dataTransfer.types.includes(DRAG_TYPE)) {
      try {
        const payload = JSON.parse(event.dataTransfer.getData(DRAG_TYPE)) as { server_id: string; paths: string[] };
        if (payload.server_id !== server.id || !Array.isArray(payload.paths) || payload.paths.some(item => typeof item !== 'string') || payload.paths.length > MAX_FILES) throw new Error('Перемещение доступно только внутри текущего сервера.');
        transfer(payload.paths, target, event.ctrlKey || event.metaKey ? 'copy' : 'move');
      } catch (error) { setFeedback({ error: message(error), success: '' }); }
    } else {
      // Snapshot browser entries before the drag event loses access to its data store.
      const collecting = droppedFiles(event.dataTransfer.items, event.dataTransfer.files);
      setReadingDrop(true);
      try {
        const items = await collecting;
        if (target !== path) { setPath(target); setSelection([]); }
        receiveUploads(items);
      } catch (error) { setFeedback({ error: message(error), success: '' }); }
      finally { setReadingDrop(false); }
    }
  };
  const shortcuts = (event: KeyboardEvent<HTMLElement>) => {
    if (opened || !(event.ctrlKey || event.metaKey) || (event.target instanceof HTMLElement && (event.target.closest('input, textarea, [contenteditable="true"]')))) return;
    const key = event.key.toLowerCase();
    if (key === 'a') { event.preventDefault(); selectAll(); }
    if ((key === 'c' || key === 'x') && selection.length && !busy) { event.preventDefault(); copy(key === 'c' ? 'copy' : 'move'); }
    if (key === 'v' && clipboard && writable) { event.preventDefault(); transfer(clipboard.paths, path, clipboard.mode); }
  };

  return <section className="file-manager" onKeyDown={shortcuts}>
    <div className="section-title"><h2>Файлы</h2><Refresh onClick={files.refresh} /></div>
    <p className="prose">Для изменения файлов остановите сервер. Копирование и перемещение сохраняют структуру каталогов; существующие файлы не перезаписываются.</p>
    <Notice error={files.error || action.error || feedback.error} success={action.success || feedback.success} />
    {pending && <Notice>Выполняется {job.data ? jobName(job.data.type).toLowerCase() : 'операция с файлами'}. <a href={`#/jobs/${enc(pending.id)}`}>Открыть журнал</a>{job.error && <span> Состояние пока недоступно: {job.error}</span>}</Notice>}
    <nav className="breadcrumbs" aria-label="Каталог сервера"><button disabled={action.busy || readingDrop} onClick={() => navigate('')}>Корень</button>{path.split('/').filter(Boolean).map((part, index, parts) => <span key={index}><ChevronRightIcon /><button disabled={action.busy || readingDrop} aria-current={index === parts.length - 1 ? 'location' : undefined} onClick={() => navigate(parts.slice(0, index + 1).join('/'))}>{part}</button></span>)}</nav>
    {opened ? <div className="editor"><div className="section-title"><h3 className="mono">{opened.path}{dirty ? ' · не сохранено' : ''}</h3><Button variant="soft" color="gray" disabled={action.busy} onClick={() => { if (canLeave()) setOpened(undefined); }}>Закрыть файл</Button></div><label htmlFor="file-content" className="sr-only">Содержимое файла {opened.path}</label><TextArea id="file-content" className="code-editor" value={draft} onChange={event => setDraft(event.target.value)} readOnly={!writable} rows={22} spellCheck={false} /><div className="actions"><Button disabled={!writable || !dirty} loading={action.busy} onClick={() => action.run(async () => { await api(`${base}/files/content`, 'PUT', { path: opened.path, content: draft, revision: opened.revision }); const result = await api<FileContent>(`${base}/files/content?path=${enc(opened.path)}`); setOpened(result); setDraft(result.content); }, 'Файл сохранен. При необходимости перезапустите сервер.')}>Сохранить файл</Button></div></div> : <>
      <div className="file-manager-toolbar" role="group" aria-label="Действия с файлами">
        <div className="actions"><Button variant="soft" disabled={!writable} onClick={() => { setName(''); setForm(form === 'directory' ? null : 'directory'); }}><PlusIcon />Новый каталог</Button><Button disabled={!writable} onClick={() => picker.current?.click()}><UploadIcon />Загрузить файлы</Button><Button variant="soft" color="gray" disabled={!writable} onClick={() => folderPicker.current?.click()}>Загрузить каталог</Button></div>
        <input ref={picker} hidden type="file" multiple tabIndex={-1} aria-label="Выбрать файлы для загрузки" onChange={event => { receiveUploads(Array.from(event.target.files || []).map(file => ({ file, name: file.name }))); event.target.value = ''; }} />
        <input ref={folderPicker} hidden type="file" multiple {...{ webkitdirectory: '' }} tabIndex={-1} aria-label="Выбрать каталог для загрузки" onChange={event => { receiveUploads(Array.from(event.target.files || []).map(file => ({ file, name: file.webkitRelativePath || file.name }))); event.target.value = ''; }} />
        <small>{canWrite ? 'Перетащите файлы или каталог сюда. До 100 файлов, каждый до 256 МиБ.' : 'Просмотр доступен. Изменения требуют прав управления, остановленного сервера и подключенного хоста.'}</small>
      </div>
      <div className="file-manager-selection" role="group" aria-label="Действия с выделением"><span aria-live="polite">{selection.length ? `Выбрано: ${selection.length}` : 'Выберите файлы или каталоги'}</span><div className="actions file-manager-icon-actions">
        {selection.length > 0 && <><FileTool label="Снять выделение" onClick={() => { setSelection([]); anchor.current = undefined; }}><Cross2Icon /></FileTool><FileTool label="Копировать" disabled={busy} onClick={() => copy('copy')}><CopyIcon /></FileTool><FileTool label="Вырезать" disabled={!writable} onClick={() => copy('move')}><ScissorsIcon /></FileTool></>}
        {clipboard && <FileTool label={`Вставить (${clipboard.paths.length})`} disabled={!writable} accent onClick={() => transfer(clipboard.paths, path, clipboard.mode)}><ClipboardIcon /><span className="file-tool-count" aria-hidden="true">{clipboard.paths.length}</span></FileTool>}
        {selection.length > 0 && <><FileTool label="Переместить" disabled={!writable} onClick={() => { setDestination(path); setForm('move'); }}><MoveIcon /></FileTool><FileTool label="Переименовать" disabled={!selectedEntry || !writable} onClick={() => { setRenamePath(selectedEntry!.path); setName(selectedEntry!.name); setForm('rename'); }}><Pencil1Icon /></FileTool><FileTool label="Распаковать ZIP или RAR" disabled={!archive || !writable} onClick={() => { setDestination(join(path, archive!.name.replace(/\.(zip|rar)$/i, ''))); setForm('extract'); }}><ArchiveIcon /></FileTool><Confirm danger iconLabel="Удалить выбранные файлы" disabled={!writable} title={`Удалить выбранные объекты (${selection.length})?`} description="Файлы и каталоги со всем содержимым будут удалены без возможности отмены." onConfirm={() => action.run(async () => { const result = await api<Job>(`${base}/files/delete-batch`, 'POST', { paths: selection }); queue(result); })}><TrashIcon /></Confirm></>}
      </div></div>
      {clipboard && <div className="file-manager-clipboard"><small>{clipboard.mode === 'copy' ? 'В буфере для копирования' : 'В буфере для перемещения'}: {clipboard.paths.length === 1 ? clipboard.paths[0] : `${clipboard.paths.length} ${quantity(clipboard.paths.length, 'объект', 'объекта', 'объектов')}`}. Вставка в /{path || ''}.</small><FileTool label="Очистить буфер" disabled={busy} onClick={() => setClipboard(undefined)}><Cross2Icon /></FileTool></div>}
      {form && <div className="file-manager-form">
        {form === 'directory' && <form className="inline-edit" onSubmit={event => { event.preventDefault(); if (!writable) return; void action.run(async () => { await api(`${base}/files/directory`, 'POST', { path: join(path, childName(name)) }); setForm(null); setName(''); files.refresh(); }, 'Каталог создан.'); }}><Input label="Название нового каталога" value={name} onChange={event => setName(event.target.value)} required autoFocus disabled={!writable} /><div className="actions"><Button type="submit" disabled={!writable || !name.trim()}>Создать каталог</Button><Button type="button" color="gray" variant="soft" disabled={busy} onClick={() => setForm(null)}>Отмена</Button></div></form>}
        {form === 'rename' && <form className="inline-edit" onSubmit={event => { event.preventDefault(); if (!writable) return; void action.run(async () => { const parent = renamePath.split('/').slice(0, -1).join('/'); await api(`${base}/files/rename`, 'POST', { path: renamePath, new_path: join(parent, childName(name)) }); setForm(null); files.refresh(); }, 'Переименовано.'); }}><Input label={`Новое имя для ${fileName(renamePath)}`} value={name} onChange={event => setName(event.target.value)} required autoFocus disabled={!writable} /><div className="actions"><Button type="submit" disabled={!writable || !name.trim()}>Переименовать</Button><Button type="button" color="gray" variant="soft" disabled={busy} onClick={() => setForm(null)}>Отмена</Button></div></form>}
        {(form === 'move' || form === 'extract') && <form className="inline-edit" onSubmit={event => { event.preventDefault(); if (!writable) return; if (form === 'move') transfer(selection, destination.replace(/^\/+|\/+$/g, ''), 'move'); else if (archive) void action.run(async () => { const result = await api<Job>(`${base}/files/extract`, 'POST', { path: archive.path, destination: destination.trim().replace(/^\/+|\/+$/g, '') }); queue(result); }); }}><Input label={form === 'move' ? 'Каталог назначения' : `Куда распаковать ${archive?.name || 'архив'}`} hint={form === 'move' ? 'Путь относительно корня сервера. Пустое поле означает корень. Каталог должен существовать.' : 'Путь относительно корня сервера. Новый каталог будет создан. Существующие файлы не перезаписываются.'} value={destination} onChange={event => setDestination(event.target.value)} autoFocus disabled={!writable} /><div className="actions"><Button type="submit" disabled={!writable || (form === 'extract' && !archive) || (form === 'move' && !selection.length)}>{form === 'move' ? 'Переместить' : 'Распаковать'}</Button><Button type="button" color="gray" variant="soft" disabled={busy} onClick={() => setForm(null)}>Отмена</Button></div></form>}
        {form === 'upload' && <div><div className="section-title"><h3>Загрузка в /{path || ''}</h3><small>{uploads.length} {quantity(uploads.length, 'файл', 'файла', 'файлов')} · {bytes(uploads.reduce((sum, item) => sum + item.file.size, 0))}</small></div><ul className="file-manager-upload-list">{uploads.map(item => <li key={item.name}><span className="mono">{item.name}</span><small>{bytes(item.file.size)}</small><Button variant="ghost" color="gray" size="1" aria-label={`Убрать ${item.name} из загрузки`} disabled={busy} onClick={() => setUploads(current => current.filter(candidate => candidate !== item))}>Убрать</Button></li>)}</ul>{progress && <div className="file-manager-progress" role="status"><span>Отправка {progress.index} из {progress.total}: {progress.name}</span><progress value={progress.loaded} max={progress.size || 1} aria-label="Прогресс загрузки текущего файла" /><small>{bytes(progress.loaded)} / {bytes(progress.size)}</small></div>}<div className="actions"><Button disabled={!writable || !uploads.length} loading={action.busy} onClick={sendUploads}>Загрузить {uploads.length} {quantity(uploads.length, 'файл', 'файла', 'файлов')}</Button><Button variant="soft" color="gray" disabled={busy} onClick={() => { setUploads([]); setForm(null); }}>Отмена</Button></div></div>}
      </div>}
      <div className={`file-manager-dropzone${dragging ? ' is-dragging' : ''}`} onDragEnter={event => { if (!writable || !event.dataTransfer.types.includes('Files')) return; event.preventDefault(); dragDepth.current++; setDragging(true); }} onDragLeave={() => { dragDepth.current = Math.max(0, dragDepth.current - 1); if (!dragDepth.current) setDragging(false); }} onDragOver={event => { if (event.dataTransfer.types.includes('Files') || event.dataTransfer.types.includes(DRAG_TYPE)) { event.preventDefault(); event.dataTransfer.dropEffect = writable ? event.ctrlKey || event.metaKey ? 'copy' : event.dataTransfer.types.includes(DRAG_TYPE) ? 'move' : 'copy' : 'none'; } }} onDrop={event => void drop(event)}>
        {dragging && <div className="file-manager-drop-overlay"><UploadIcon /><strong>Отпустите, чтобы добавить файлы</strong><span>Каталог: /{dropFolder || path || ''}</span></div>}
        {readingDrop ? <Loading /> : files.loading ? <Loading /> : files.data === undefined ? <Unavailable onRetry={files.refresh} /> : entries.length ? <div className="table-wrap file-manager-table"><Table.Root><Table.Header><Table.Row><Table.ColumnHeaderCell className="file-manager-check"><Checkbox aria-label="Выделить все файлы и каталоги" checked={allSelected ? true : selection.length ? 'indeterminate' : false} onCheckedChange={selectAll} /></Table.ColumnHeaderCell><Table.ColumnHeaderCell>Название</Table.ColumnHeaderCell><Table.ColumnHeaderCell className="file-manager-size">Размер</Table.ColumnHeaderCell><Table.ColumnHeaderCell className="file-manager-date">Изменен</Table.ColumnHeaderCell></Table.Row></Table.Header><Table.Body>{entries.map(file => <Table.Row key={file.path} data-selected={selection.includes(file.path) || undefined} data-cut={clipboard?.mode === 'move' && clipboard.paths.includes(file.path) || undefined} data-drop-target={dropFolder === file.path || undefined} draggable={writable} onDragStart={event => { if (!writable) { event.preventDefault(); return; } const paths = selection.includes(file.path) ? selection : [file.path]; event.dataTransfer.setData(DRAG_TYPE, JSON.stringify({ server_id: server.id, paths })); event.dataTransfer.effectAllowed = 'copyMove'; }} onDragEnd={() => setDropFolder('')} onDragOver={event => { if (file.is_dir && writable && (event.dataTransfer.types.includes(DRAG_TYPE) || event.dataTransfer.types.includes('Files'))) { event.preventDefault(); event.stopPropagation(); setDropFolder(file.path); event.dataTransfer.dropEffect = event.ctrlKey || event.metaKey || event.dataTransfer.types.includes('Files') ? 'copy' : 'move'; } }} onDragLeave={event => { if (!event.currentTarget.contains(event.relatedTarget as globalThis.Node | null)) setDropFolder(''); }} onDrop={event => { if (file.is_dir) void drop(event, file.path); }}><Table.Cell className="file-manager-check"><Checkbox aria-label={`Выбрать ${file.name}`} checked={selection.includes(file.path)} onClick={event => { if (event.shiftKey) { event.preventDefault(); toggle(file, !selection.includes(file.path), true); } }} onCheckedChange={checked => toggle(file, checked === true)} /></Table.Cell><Table.RowHeaderCell><button className="file-link" disabled={busy} onClick={() => open(file)}>{file.is_dir ? <ArchiveIcon /> : <FileIcon />}<span>{file.name}</span></button></Table.RowHeaderCell><Table.Cell className="file-manager-size">{file.is_dir ? 'Каталог' : bytes(file.size)}</Table.Cell><Table.Cell className="file-manager-date">{date(file.modified)}</Table.Cell></Table.Row>)}</Table.Body></Table.Root></div> : <Empty title="Каталог пуст">Добавьте файлы кнопкой «Загрузить файлы», перетащите их сюда или создайте новый каталог.</Empty>}
      </div>
      <small className="file-manager-help">Shift + выбор выделяет диапазон. Ctrl/Cmd + C, X, V: копирование, вырезание, вставка. Перетаскивание в каталог перемещает файлы; с Ctrl/Cmd копирует. ZIP и RAR распаковываются через кнопку «Распаковать».</small>
    </>}
  </section>;
}
