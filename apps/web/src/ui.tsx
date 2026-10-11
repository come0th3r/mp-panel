import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { AlertDialog, Badge, Button, Callout, Heading, Select, Skeleton, TextField } from '@radix-ui/themes';
import { InfoCircledIcon, ReloadIcon } from '@radix-ui/react-icons';
import { api } from './api';

export function useResource<T>(path: string | null, interval = 0) {
  const [data, setData] = useState<T>(); const [error, setError] = useState(''); const [loading, setLoading] = useState(!!path); const [revision, setRevision] = useState(0);
  const currentPath = useRef<string | null>(null);
  useEffect(() => {
    if (!path) { currentPath.current = null; setData(undefined); setLoading(false); setError(''); return; }
    const controller = new AbortController(); let running = false; let initial = true;
    const changed = currentPath.current !== path; currentPath.current = path;
    if (changed) setData(undefined); setError(''); setLoading(changed || data === undefined);
    const run = async () => { if (running) return; running = true; try { const result = await api<T>(path, 'GET', undefined, controller.signal); if (!controller.signal.aborted) { setData(result); setError(''); } } catch (e) { if (!controller.signal.aborted) setError(message(e)); } finally { if (!controller.signal.aborted && initial) { setLoading(false); initial = false; } running = false; } };
    void run(); const timer = interval ? window.setInterval(run, interval) : undefined;
    return () => { controller.abort(); if (timer) clearInterval(timer); };
  }, [path, interval, revision]);
  return { data, error, loading, refresh: useCallback(() => setRevision(x => x + 1), []) };
}
export function message(error: unknown) { return error instanceof Error ? error.message : 'Не удалось выполнить действие.'; }
export function useAction() {
  const [busy, setBusy] = useState(false); const [error, setError] = useState(''); const [success, setSuccess] = useState(''); const lock = useRef(false);
  const run = async (task: () => Promise<void>, ok = '') => { if (lock.current) return; lock.current = true; setBusy(true); setError(''); setSuccess(''); try { await task(); setSuccess(ok); } catch (e) { setError(message(e)); } finally { lock.current = false; setBusy(false); } };
  return { busy, error, success, run, clear: () => { setError(''); setSuccess(''); } };
}
export function useUnsavedChanges(dirty: boolean) {
  useEffect(() => {
    if (!dirty) return;
    const unload = (event: BeforeUnloadEvent) => event.preventDefault();
    const navigate = (event: Event) => { if (!window.confirm('Несохраненные изменения будут потеряны. Продолжить?')) event.preventDefault(); };
    window.addEventListener('beforeunload', unload);
    window.addEventListener('mp:before-navigate', navigate);
    return () => { window.removeEventListener('beforeunload', unload); window.removeEventListener('mp:before-navigate', navigate); };
  }, [dirty]);
}
export function allowNavigation() { return window.dispatchEvent(new Event('mp:before-navigate', { cancelable: true })); }
export function Notice({ error, success, children }: { error?: string; success?: string; children?: ReactNode }) {
  if (!error && !success && !children) return null;
  return <Callout.Root color={error ? 'red' : success ? 'green' : 'gray'} role={error ? 'alert' : 'status'} className="notice"><Callout.Icon><InfoCircledIcon /></Callout.Icon><Callout.Text>{error || success || children}</Callout.Text></Callout.Root>;
}
export function Loading() { return <div aria-label="Загрузка данных" aria-busy="true" className="loading"><Skeleton height="24px" width="200px" /><Skeleton height="72px" /><Skeleton height="72px" /></div>; }
export function Empty({ title, children, action }: { title: string; children: ReactNode; action?: ReactNode }) { return <div className="empty"><Heading size="4">{title}</Heading><p>{children}</p>{action}</div>; }
export function Unavailable({ onRetry }: { onRetry: () => void }) { return <Empty title="Данные недоступны" action={<Refresh onClick={onRetry} />}>Не удалось получить данные панели. Повторите запрос после восстановления соединения.</Empty>; }
export function PageTitle({ title, description, action }: { title: string; description?: string; action?: ReactNode }) { return <header className="page-title"><div><h1>{title}</h1>{description && <p>{description}</p>}</div>{action && <div className="actions">{action}</div>}</header>; }
export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) { const id = useId(); return <div className="field"><div id={id} className="field-label">{label}</div><div aria-labelledby={id}>{children}</div>{hint && <small>{hint}</small>}</div>; }
export function Input({ label, hint, ...props }: React.ComponentProps<typeof TextField.Root> & { label: string; hint?: string }) { const id = useId(); return <div className="field"><label htmlFor={id}>{label}</label><TextField.Root id={id} aria-describedby={hint ? `${id}-hint` : undefined} size="3" {...props} />{hint && <small id={`${id}-hint`}>{hint}</small>}</div>; }
export function Choice({ label, hint, value, onChange, items, disabled }: { label: string; hint?: string; value: string; onChange: (value: string) => void; items: { value: string; label: string; disabled?: boolean }[]; disabled?: boolean }) { const id = useId(); return <div className="field"><label id={id}>{label}</label><Select.Root value={value || undefined} onValueChange={onChange} disabled={disabled}><Select.Trigger aria-labelledby={id} aria-describedby={hint ? `${id}-hint` : undefined} placeholder="Выберите значение" className="select" /><Select.Content>{items.map(item => <Select.Item value={item.value} key={item.value} disabled={item.disabled}>{item.label}</Select.Item>)}</Select.Content></Select.Root>{hint && <small id={`${id}-hint`}>{hint}</small>}</div>; }
export function Confirm({ title, description, children, onConfirm, disabled, danger = false }: { title: string; description: string; children: ReactNode; onConfirm: () => void; disabled?: boolean; danger?: boolean }) { return <AlertDialog.Root><AlertDialog.Trigger><Button disabled={disabled} variant="soft" color={danger ? 'red' : 'gray'}>{children}</Button></AlertDialog.Trigger><AlertDialog.Content maxWidth="460px"><AlertDialog.Title>{title}</AlertDialog.Title><AlertDialog.Description>{description}</AlertDialog.Description><div className="actions dialog-actions"><AlertDialog.Cancel><Button variant="soft" color="gray">Отмена</Button></AlertDialog.Cancel><AlertDialog.Action><Button color={danger ? 'red' : 'violet'} onClick={onConfirm}>Подтвердить</Button></AlertDialog.Action></div></AlertDialog.Content></AlertDialog.Root>; }
const statuses: Record<string, string> = { creating: 'Создается', updating: 'Обновляется', restoring: 'Восстанавливается', active: 'Правило применено', pending: 'Ожидает применения', removing: 'Правило удаляется', unknown: 'Состояние неизвестно', online: 'На связи', offline: 'Нет связи', connecting: 'Подключается', error: 'Ошибка', running: 'Работает', stopped: 'Остановлен', starting: 'Запускается', stopping: 'Останавливается', installing: 'Устанавливается', queued: 'В очереди', succeeded: 'Завершено', failed: 'Ошибка', deleting: 'Удаляется' };
export function jobName(value: string) { return ({ bootstrap: 'Подключение хоста', install: 'Установка сервера', start: 'Запуск сервера', stop: 'Остановка сервера', restart: 'Перезапуск сервера', delete: 'Удаление сервера', backup: 'Резервное копирование', restore: 'Восстановление копии', upload: 'Загрузка файла', extension: 'Установка расширения', installation: 'Смена ядра', reinstall: 'Смена ядра', 'file.upload': 'Загрузка файла', 'file.upload_batch': 'Загрузка файлов', 'file.transfer': 'Копирование или перемещение файлов', 'file.extract': 'Распаковка архива', 'file.delete_batch': 'Удаление файлов' } as Record<string, string>)[value] || value; }
export function Status({ value }: { value: string }) { const color = ['active', 'online', 'running', 'succeeded'].includes(value) ? 'green' : ['error', 'failed'].includes(value) ? 'red' : ['creating', 'updating', 'restoring', 'pending', 'removing', 'queued', 'installing', 'connecting', 'starting', 'stopping', 'deleting'].includes(value) ? 'amber' : 'gray'; return <Badge color={color} variant="soft">{statuses[value] || value || 'Неизвестно'}</Badge>; }
export function Refresh({ onClick }: { onClick: () => void }) { return <Button variant="soft" color="gray" onClick={onClick}><ReloadIcon />Обновить</Button>; }
export function bytes(value?: number) { if (value === undefined) return 'Нет данных'; if (value === 0) return '0 Б'; const units = ['Б', 'КиБ', 'МиБ', 'ГиБ', 'ТиБ']; const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1); return `${(value / 1024 ** index).toLocaleString('ru-RU', { maximumFractionDigits: 1 })} ${units[index]}`; }
export function date(value?: string) { return value ? new Date(value).toLocaleString('ru-RU') : 'Нет данных'; }
export function go(path: string) { window.location.hash = path; }
