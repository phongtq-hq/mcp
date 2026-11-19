// Chỗ duy nhất gọi Management API. Giao diện không tự quyết định quyền: API trả 403 / 404
// thì màn hình hiện thông báo tương ứng.
import * as auth from '@/auth/oidc';
import { t } from '@/i18n';
import { ApiError, type ProblemLike } from '@/lib/api-error';
import type { components } from './schema.gen';

export { ApiError };

// Kiểu sinh từ .docs/api/openapi.yaml (`make generate`); không khai báo lại bằng tay.
export type Schemas = components['schemas'];
export interface Page<T> { data: T[]; next_cursor: string | null }

type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';

export async function api<T>(method: Method, path: string, body?: unknown): Promise<T> {
  const token = await auth.accessToken();
  if (!token) { auth.expireSession(); throw new ApiError(401, { detail: t('errSession') }); }
  const headers: Record<string, string> = { Authorization: 'Bearer ' + token, Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  // Mọi POST tạo tài nguyên / kích hoạt hành động mang Idempotency-Key: bấm hai lần
  // (hoặc trình duyệt gửi lại) không tạo hai lần chạy.
  if (method === 'POST') headers['Idempotency-Key'] = crypto.randomUUID();
  let res: Response;
  try {
    res = await fetch(auth.apiBase() + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError(0, { detail: t('errNetwork') });
  }
  if (res.status === 401) { auth.expireSession(); throw new ApiError(401, { detail: t('errSession') }); }
  if (res.status === 204) return null as T;
  let data: unknown;
  try { data = await res.json(); } catch { data = null; }
  if (!res.ok) throw new ApiError(res.status, data && typeof data === 'object' ? data as ProblemLike : null);
  return data as T;
}

export function get<T>(path: string): Promise<T> { return api<T>('GET', path); }

// soft: một lời gọi phụ thất bại (thường do thiếu quyền) không làm hỏng cả màn hình.
export async function soft<T>(promise: Promise<T>): Promise<T | null> {
  try { return await promise; } catch { return null; }
}
