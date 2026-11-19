// Đăng nhập OIDC: Authorization Code + PKCE với một public client. Trình duyệt không giữ
// client secret nào. Token nằm trong sessionStorage (mất khi đóng tab); mọi quyền vẫn do
// API kiểm trên từng request.
import { useSyncExternalStore } from 'react';

const KEY = 'hq.session';
const PENDING = 'hq.login';

interface PublicConfig { issuer: string; client_id: string; api_base: string }
interface Endpoints { authorization_endpoint: string; token_endpoint: string; end_session_endpoint: string }
interface Session { access_token: string; refresh_token: string | null; id_token: string | null; expires_at: number }
interface TokenResponse { access_token: string; refresh_token?: string; id_token?: string; expires_in?: number | string }
interface PendingLogin { verifier: string; state: string; back: string }

// Lý do hiện ở màn hình đăng nhập. Chỉ là mã: văn bản lấy từ từ điển, không từ URL hay IdP.
export type SignInNotice = 'session' | 'state' | 'failed' | null;
export interface AuthState { signedIn: boolean; notice: SignInNotice }

let config: PublicConfig | null = null;
let endpoints: Endpoints | null = null;
let session: Session | null = null;

// ---------------------------------------------------------------- Trạng thái cho React

let state: AuthState = { signedIn: false, notice: null };
const listeners = new Set<() => void>();

function publish(next: AuthState): void {
  if (next.signedIn === state.signedIn && next.notice === state.notice) return;
  state = next;
  for (const l of listeners) l();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function useAuthState(): AuthState {
  return useSyncExternalStore(subscribe, () => state);
}

export function setNotice(notice: SignInNotice): void {
  publish({ signedIn: state.signedIn, notice });
}

// ---------------------------------------------------------------- Tiện ích

function b64url(bytes: Uint8Array): string {
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
}

function randomString(n: number): string {
  const a = new Uint8Array(n);
  crypto.getRandomValues(a);
  return b64url(a);
}

function redirectURI(): string { return location.origin + '/ui/'; }

function need<T>(value: T | null, what: string): T {
  if (value === null) throw new Error('auth: ' + what + ' chưa được nạp');
  return value;
}

// ---------------------------------------------------------------- Cấu hình và phiên

export async function loadConfig(): Promise<void> {
  const res = await fetch('config.json', { cache: 'no-store' });
  if (!res.ok) throw new Error('config ' + res.status);
  const cfg = await res.json() as PublicConfig;
  if (typeof cfg.issuer !== 'string' || typeof cfg.client_id !== 'string' || typeof cfg.api_base !== 'string') throw new Error('config: sai dạng');
  config = cfg;
  // Điểm cuối chuẩn của Keycloak làm mặc định; metadata của IdP (nếu đọc được) ghi đè.
  const base = cfg.issuer + '/protocol/openid-connect';
  const eps: Endpoints = { authorization_endpoint: base + '/auth', token_endpoint: base + '/token', end_session_endpoint: base + '/logout' };
  try {
    const meta = await fetch(cfg.issuer + '/.well-known/openid-configuration');
    if (meta.ok) {
      const m = await meta.json() as Record<string, unknown>;
      // Chỉ nhận điểm cuối nằm trên chính origin của issuer đã cấu hình.
      const origin = new URL(cfg.issuer).origin;
      for (const k of Object.keys(eps) as (keyof Endpoints)[]) {
        const v = m[k];
        if (typeof v === 'string' && new URL(v).origin === origin) eps[k] = v;
      }
    }
  } catch { /* dùng mặc định */ }
  endpoints = eps;
  try { session = JSON.parse(sessionStorage.getItem(KEY) || 'null') as Session | null; } catch { session = null; }
  publish({ signedIn: isSignedIn(), notice: state.notice });
}

export function apiBase(): string { return need(config, 'cấu hình').api_base; }

export function isSignedIn(): boolean { return !!(session && session.access_token); }

export interface Claims { sub?: string; preferred_username?: string; email?: string }

// claims đọc phần thân của access token để HIỂN THỊ (tên, email). Không dùng để quyết
// định quyền: chữ ký không được kiểm ở trình duyệt, API mới là nơi kiểm.
export function claims(): Claims {
  if (!session) return {};
  try {
    const p = (session.access_token.split('.')[1] ?? '').replaceAll('-', '+').replaceAll('_', '/');
    const bin = atob(p + '='.repeat((4 - p.length % 4) % 4));
    const bytes = Uint8Array.from(bin, c => c.charCodeAt(0));
    const parsed: unknown = JSON.parse(new TextDecoder().decode(bytes));
    if (!parsed || typeof parsed !== 'object') return {};
    const c = parsed as Record<string, unknown>;
    const str = (v: unknown) => (typeof v === 'string' ? v : undefined);
    return { sub: str(c.sub), preferred_username: str(c.preferred_username), email: str(c.email) };
  } catch { return {}; }
}

function store(tok: TokenResponse): void {
  session = {
    access_token: tok.access_token,
    refresh_token: tok.refresh_token || (session && session.refresh_token) || null,
    id_token: tok.id_token || (session && session.id_token) || null,
    expires_at: Date.now() + (Number(tok.expires_in) || 60) * 1000,
  };
  sessionStorage.setItem(KEY, JSON.stringify(session));
  publish({ signedIn: true, notice: null });
}

function clear(notice: SignInNotice = state.notice): void {
  session = null;
  try { sessionStorage.removeItem(KEY); } catch { /* bỏ qua */ }
  publish({ signedIn: false, notice });
}

// ---------------------------------------------------------------- Đăng nhập

export async function login(): Promise<void> {
  const cfg = need(config, 'cấu hình');
  const verifier = randomString(48);
  const loginState = randomString(24);
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier));
  const pending: PendingLogin = { verifier, state: loginState, back: location.hash || '#/' };
  sessionStorage.setItem(PENDING, JSON.stringify(pending));
  const q = new URLSearchParams({
    response_type: 'code', client_id: cfg.client_id, redirect_uri: redirectURI(), scope: 'openid',
    state: loginState, code_challenge: b64url(new Uint8Array(digest)), code_challenge_method: 'S256',
  });
  location.assign(need(endpoints, 'điểm cuối').authorization_endpoint + '?' + q.toString());
}

async function tokenRequest(params: Record<string, string>): Promise<TokenResponse> {
  const res = await fetch(need(endpoints, 'điểm cuối').token_endpoint, {
    method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ client_id: need(config, 'cấu hình').client_id, ...params }),
  });
  if (!res.ok) throw new Error('token ' + res.status);
  const tok = await res.json() as TokenResponse;
  if (typeof tok.access_token !== 'string' || !tok.access_token) throw new Error('token: thiếu access_token');
  return tok;
}

export interface Callback { code: string | null; state: string | null; failed: boolean; pending: PendingLogin | null }

// takeCallback đọc tham số của lượt quay về từ IdP (?code=…&state=… hoặc ?error=…) và
// xoá chúng khỏi thanh địa chỉ và lịch sử NGAY (đồng bộ, trước mọi lời gọi mạng). Trả
// null khi trang không được mở bởi lượt quay về.
export function takeCallback(): Callback | null {
  const q = new URLSearchParams(location.search);
  if (!q.has('code') && !q.has('error')) return null;
  let pending: PendingLogin | null;
  try { pending = JSON.parse(sessionStorage.getItem(PENDING) || 'null') as PendingLogin | null; } catch { pending = null; }
  try { sessionStorage.removeItem(PENDING); } catch { /* bỏ qua */ }
  const back = pending && typeof pending.back === 'string' && pending.back.startsWith('#/') ? pending.back : '#/';
  history.replaceState(null, '', location.pathname + back);
  return { code: q.get('code'), state: q.get('state'), failed: q.has('error'), pending };
}

export type LoginResult = null | { ok: true } | { ok: false; reason: 'state' | 'idp' | 'token' };

// completeLogin đổi code lấy token. Trả về null (không phải lượt quay về), {ok: true}
// hoặc {ok: false, reason}. Văn bản lỗi trong URL (error_description) KHÔNG được dùng:
// ai cũng dựng được một liên kết mang văn bản tuỳ ý tới trang đăng nhập này. Một lượt
// quay về không khớp với lượt đăng nhập đang chờ của chính tab này bị coi là không có.
export async function completeLogin(cb: Callback | null): Promise<LoginResult> {
  if (!cb) return null;
  if (!cb.pending || !cb.state || cb.pending.state !== cb.state) return cb.failed ? null : { ok: false, reason: 'state' };
  if (cb.failed || !cb.code) return { ok: false, reason: 'idp' };
  try {
    store(await tokenRequest({ grant_type: 'authorization_code', code: cb.code, redirect_uri: redirectURI(), code_verifier: cb.pending.verifier }));
    return { ok: true };
  } catch {
    return { ok: false, reason: 'token' };
  }
}

let refreshing: Promise<void> | null = null;

// accessToken trả về token còn hạn, làm mới nếu sắp hết. Trả null khi phải đăng nhập lại.
export async function accessToken(): Promise<string | null> {
  if (!session) return null;
  if (session.expires_at - Date.now() > 20_000) return session.access_token;
  if (!session.refresh_token) { clear(); return null; }
  if (!refreshing) {
    refreshing = tokenRequest({ grant_type: 'refresh_token', refresh_token: session.refresh_token })
      .then(store, () => clear()).finally(() => { refreshing = null; });
  }
  await refreshing;
  const now: Session | null = session;
  return now ? now.access_token : null;
}

// expireSession: API từ chối token (401) hoặc không còn token: bỏ phiên và báo lý do ở
// màn hình đăng nhập.
export function expireSession(): void { clear('session'); }

export function logout(): void {
  const cfg = need(config, 'cấu hình');
  const idToken = session && session.id_token;
  clear(null);
  const q = new URLSearchParams({ client_id: cfg.client_id, post_logout_redirect_uri: redirectURI() });
  if (idToken) q.set('id_token_hint', idToken);
  location.assign(need(endpoints, 'điểm cuối').end_session_endpoint + '?' + q.toString());
}
