import { beforeEach, describe, expect, it, vi } from 'vitest';

const auth = vi.hoisted(() => ({
  accessToken: vi.fn<() => Promise<string | null>>(),
  apiBase: vi.fn(() => '/v1'),
  expireSession: vi.fn(),
}));
vi.mock('@/auth/oidc', () => auth);

import { api, ApiError, get, soft } from './client';

function respond(status: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

const fetchMock = vi.fn<(url: string, init: RequestInit) => Promise<Response>>();

beforeEach(() => {
  fetchMock.mockReset();
  auth.accessToken.mockReset().mockResolvedValue('token-1');
  auth.expireSession.mockReset();
  vi.stubGlobal('fetch', fetchMock);
});

const headersOf = (call: number) => fetchMock.mock.calls[call]![1].headers as Record<string, string>;

describe('api', () => {
  it('gửi token của người dùng tới đường dẫn dưới api_base', async () => {
    fetchMock.mockResolvedValue(respond(200, { id: 'p1' }));
    await expect(get<{ id: string }>('/projects/p1')).resolves.toEqual({ id: 'p1' });
    expect(fetchMock.mock.calls[0]![0]).toBe('/v1/projects/p1');
    expect(headersOf(0).Authorization).toBe('Bearer token-1');
  });

  it('mọi POST mang Idempotency-Key, mỗi lần một khoá; GET thì không', async () => {
    fetchMock.mockImplementation(async () => respond(200, {}));
    await api('POST', '/approvals/a1/decisions', { decision: 'reject' });
    await api('POST', '/approvals/a1/decisions', { decision: 'reject' });
    await get('/approvals');
    const first = headersOf(0)['Idempotency-Key'];
    const second = headersOf(1)['Idempotency-Key'];
    expect(first).toMatch(/^[0-9a-f-]{36}$/);
    expect(second).toMatch(/^[0-9a-f-]{36}$/);
    expect(first).not.toBe(second);
    expect(headersOf(2)['Idempotency-Key']).toBeUndefined();
    expect(fetchMock.mock.calls[0]![1].body).toBe('{"decision":"reject"}');
  });

  it('401 kết thúc phiên', async () => {
    fetchMock.mockResolvedValue(respond(401, { title: 'Unauthorized' }));
    await expect(get('/organization')).rejects.toMatchObject({ status: 401 });
    expect(auth.expireSession).toHaveBeenCalledTimes(1);
  });

  it('không còn token thì không gọi mạng và kết thúc phiên', async () => {
    auth.accessToken.mockResolvedValue(null);
    await expect(get('/organization')).rejects.toBeInstanceOf(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
    expect(auth.expireSession).toHaveBeenCalledTimes(1);
  });

  it('lỗi RFC 9457 thành ApiError mang status, code và detail', async () => {
    fetchMock.mockResolvedValue(respond(403, { title: 'Forbidden', detail: 'thiếu quyền approval:decide', code: 'permission_denied' }));
    const err = await get('/approvals').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 403, code: 'permission_denied', message: 'thiếu quyền approval:decide' });
    expect(auth.expireSession).not.toHaveBeenCalled();
  });

  it('thân lỗi không phải JSON vẫn thành ApiError', async () => {
    fetchMock.mockResolvedValue(new Response('<html>bad gateway</html>', { status: 502 }));
    await expect(get('/projects')).rejects.toMatchObject({ status: 502, message: 'HTTP 502' });
  });

  it('không tới được máy chủ: status 0', async () => {
    fetchMock.mockRejectedValue(new TypeError('failed to fetch'));
    await expect(get('/projects')).rejects.toMatchObject({ status: 0 });
  });

  it('204 trả null', async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await expect(api('DELETE', '/api-keys/k1')).resolves.toBeNull();
  });
});

describe('soft', () => {
  it('nuốt lỗi của lời gọi phụ', async () => {
    await expect(soft(Promise.reject(new ApiError(403, null)))).resolves.toBeNull();
    await expect(soft(Promise.resolve(1))).resolves.toBe(1);
  });
});
