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

export interface RequestOptions {
  // ETag của bản đã đọc. Gửi kèm PATCH / DELETE để máy chủ từ chối (412) khi tài nguyên
  // đã bị sửa ở nơi khác kể từ lúc đọc, thay vì lặng lẽ ghi đè.
  ifMatch?: string | null;
}

// Tài nguyên kèm ETag của nó (null khi máy chủ không gửi).
export interface WithETag<T> { data: T; etag: string | null }

// Gửi lại sau một lần thất bại KHÔNG RÕ KẾT QUẢ (mất mạng, 5xx): request đầu có thể đã tới
// máy chủ và đã được thực hiện. Lần gửi lại đúng request đó (cùng đường dẫn, cùng thân) phải
// mang lại Idempotency-Key cũ để máy chủ trả kết quả cũ thay vì làm lần hai. Thân chỉ được
// giữ dưới dạng băm (nó có thể chứa khoá API); số mục và thời gian giữ đều có trần.
const RETRY_KEY_TTL_MS = 10 * 60 * 1000;
const MAX_RETRY_KEYS = 32;
const retryKeys = new Map<string, { key: string; at: number }>();

async function signature(path: string, body: string | undefined): Promise<string> {
  const bytes = new TextEncoder().encode(path + '\n' + (body ?? ''));
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return Array.from(new Uint8Array(digest), b => b.toString(16).padStart(2, '0')).join('');
}

function idempotencyKey(sig: string): string {
  const kept = retryKeys.get(sig);
  if (kept && Date.now() - kept.at < RETRY_KEY_TTL_MS) return kept.key;
  return crypto.randomUUID();
}

// settle ghi nhận kết cục của một POST: không rõ kết quả thì giữ khoá cho lần gửi lại, còn
// lại (thành công, hoặc máy chủ đã trả lời từ chối) thì bỏ.
function settle(sig: string, key: string, ambiguous: boolean): void {
  retryKeys.delete(sig);
  if (!ambiguous) return;
  if (retryKeys.size >= MAX_RETRY_KEYS) {
    const oldest = retryKeys.keys().next();
    if (!oldest.done) retryKeys.delete(oldest.value);
  }
  retryKeys.set(sig, { key, at: Date.now() });
}

// Chỉ dùng trong test: quên mọi khoá đang giữ.
export function resetRetryKeys(): void { retryKeys.clear(); }

async function request<T>(method: Method, path: string, body?: unknown, options?: RequestOptions): Promise<WithETag<T>> {
  const token = await auth.accessToken();
  if (!token) { auth.expireSession(); throw new ApiError(401, { detail: t('errSession') }); }
  const headers: Record<string, string> = { Authorization: 'Bearer ' + token, Accept: 'application/json' };
  const payload = body === undefined ? undefined : JSON.stringify(body);
  if (payload !== undefined) headers['Content-Type'] = 'application/json';
  // Mọi POST tạo tài nguyên / kích hoạt hành động mang Idempotency-Key: bấm hai lần
  // (hoặc trình duyệt gửi lại) không tạo hai lần chạy.
  let retry: { sig: string; key: string } | null = null;
  if (method === 'POST') {
    const sig = await signature(path, payload);
    retry = { sig, key: idempotencyKey(sig) };
    headers['Idempotency-Key'] = retry.key;
  }
  if (options?.ifMatch) headers['If-Match'] = options.ifMatch;
  let res: Response;
  try {
    res = await fetch(auth.apiBase() + path, { method, headers, body: payload });
  } catch {
    if (retry) settle(retry.sig, retry.key, true);
    throw new ApiError(0, { detail: t('errNetwork') });
  }
  if (retry) settle(retry.sig, retry.key, res.status >= 500);
  if (res.status === 401) { auth.expireSession(); throw new ApiError(401, { detail: t('errSession') }); }
  const etag = res.headers.get('ETag');
  if (res.status === 204) return { data: null as T, etag };
  let data: unknown;
  try { data = await res.json(); } catch { data = null; }
  if (!res.ok) throw new ApiError(res.status, data && typeof data === 'object' ? data as ProblemLike : null);
  return { data: data as T, etag };
}

export async function api<T>(method: Method, path: string, body?: unknown, options?: RequestOptions): Promise<T> {
  return (await request<T>(method, path, body, options)).data;
}

export function get<T>(path: string): Promise<T> { return api<T>('GET', path); }

// getWithETag: đọc một tài nguyên sắp được sửa, giữ ETag để gửi lại ở If-Match.
export function getWithETag<T>(path: string): Promise<WithETag<T>> { return request<T>('GET', path); }

// soft: một lời gọi phụ thất bại (thường do thiếu quyền) không làm hỏng cả màn hình.
export async function soft<T>(promise: Promise<T>): Promise<T | null> {
  try { return await promise; } catch { return null; }
}
