export type User = { id: string; username: string; role: 'owner' | 'admin' | 'operator' | 'viewer'; totp_enabled?: boolean; created_at?: string };
export type AuthStatus = { initialized: boolean; authenticated: boolean; user?: User; csrf_token?: string; setup_token_required?: boolean };
export type Node = { id: string; name: string; address: string; game_bind_address?: string; status: string; ssh_port: number; ssh_user: string; os?: string; arch?: string; cpu_count?: number; memory_total?: number; disk_total?: number; disk_free?: number; last_seen?: string; error?: string; created_at: string };
export type Server = { id: string; node_id: string; name: string; core: string; minecraft_version: string; loader_version?: string; build?: string; java_version: number; memory_mb: number; cpu_limit: number; port: number; status: string; desired_state: string; eula_accepted: boolean; created_at: string; error?: string };
export type Job = { id: string; node_id: string; server_id?: string; type: string; status: string; step: string; error?: string; logs: string[]; created_at: string; updated_at: string };
export type Metric = { timestamp: string; node_id: string; server_id?: string; cpu_percent: number; memory_used: number; memory_limit: number; disk_used: number; disk_free: number; io_read: number; io_write: number; uptime_seconds?: number };
export type Core = { id: string; name: string; description: string; kind: string };
export type Version = { id: string; stable: boolean; supported: boolean; note?: string };
export type Build = { id: string; name: string; stable: boolean; recommended: boolean; java_version: number; installer_version?: string };
export type FileEntry = { name: string; path: string; is_dir: boolean; size: number; modified?: string };
export type Backup = { id: string; name: string; server_id: string; size: number; sha256: string; created_at: string; path: string };
export type Upload = { id: string; name: string; size: number; sha256: string };
export class APIError extends Error { code?: string; status: number; constructor(message: string, status: number, code?: string) { super(message); this.status = status; this.code = code; } }
let csrf = '';
export function rememberAuth(status: AuthStatus) { csrf = status.csrf_token || ''; }
export async function api<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = {};
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  if (body && !(body instanceof FormData)) headers['Content-Type'] = 'application/json';
  const response = await fetch(`/api/v1${path}`, { method, headers, credentials: 'same-origin', body: body instanceof FormData ? body : body === undefined ? undefined : JSON.stringify(body), signal });
  const raw = await response.text();
  let value: unknown;
  try { value = raw ? JSON.parse(raw) : undefined; } catch { throw new APIError('Сервер вернул некорректный ответ. Проверьте доступность API.', response.status); }
  if (!response.ok) {
    const detail = value as { error?: string; code?: string } | undefined;
    if (response.status === 401 && !path.startsWith('/auth/')) window.dispatchEvent(new Event('mp:session-expired'));
    throw new APIError(detail?.error || `Запрос не выполнен (${response.status})`, response.status, detail?.code);
  }
  return value as T;
}
export async function upload(file: File) { if (file.size > 256 * 1024 * 1024) throw new Error('Файл превышает лимит 256 МиБ.'); const data = new FormData(); data.append('file', file); return api<Upload>('/uploads', 'POST', data); }
export const enc = encodeURIComponent;
