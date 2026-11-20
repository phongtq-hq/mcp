import { beforeEach, describe, expect, it, vi } from 'vitest';

const auth = vi.hoisted(() => ({
  accessToken: vi.fn<() => Promise<string | null>>(),
  apiBase: vi.fn(() => '/v1'),
  expireSession: vi.fn(),
}));
vi.mock('@/auth/oidc', () => auth);

import { api, ApiError, get, getWithETag, resetRetryKeys, soft } from './client';

function respond(status: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

const fetchMock = vi.fn<(url: string, init: RequestInit) => Promise<Response>>();

beforeEach(() => {
  fetchMock.mockReset();
  auth.accessToken.mockReset().mockResolvedValue('token-1');
  auth.expireSession.mockReset();
  vi.stubGlobal('fetch', fetchMock);
  resetRetryKeys();
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

  it('gửi lại đúng request sau lỗi không rõ kết quả (mất mạng, 5xx) dùng lại Idempotency-Key', async () => {
    const body = { slug: 'ops', name: 'Ops' };
    fetchMock.mockRejectedValueOnce(new TypeError('failed to fetch'));
    await expect(api('POST', '/projects', body)).rejects.toMatchObject({ status: 0 });
    fetchMock.mockResolvedValueOnce(respond(503, { title: 'Service Unavailable' }));
    await expect(api('POST', '/projects', body)).rejects.toMatchObject({ status: 503 });
    fetchMock.mockResolvedValueOnce(respond(201, { id: 'p1' }));
    await api('POST', '/projects', body);
    const keys = [0, 1, 2].map(i => headersOf(i)['Idempotency-Key']);
    expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).toBe(keys[0]);
    // Đã thành công: lần tạo kế tiếp, dù cùng nội dung, là một việc mới.
    fetchMock.mockResolvedValueOnce(respond(201, { id: 'p2' }));
    await api('POST', '/projects', body);
    expect(headersOf(3)['Idempotency-Key']).not.toBe(keys[0]);
  });

  it('không dùng lại khoá khi nội dung hoặc đường dẫn đổi, hoặc khi máy chủ đã trả lời từ chối', async () => {
    fetchMock.mockRejectedValueOnce(new TypeError('failed to fetch'));
    await expect(api('POST', '/projects', { slug: 'ops' })).rejects.toMatchObject({ status: 0 });
    fetchMock.mockImplementation(async () => respond(422, { title: 'Unprocessable', code: 'validation_failed' }));
    await expect(api('POST', '/projects', { slug: 'ops-2' })).rejects.toMatchObject({ status: 422 });
    await expect(api('POST', '/budgets', { slug: 'ops' })).rejects.toMatchObject({ status: 422 });
    // 422 là câu trả lời dứt khoát: sửa xong gửi lại cùng thân cũng là khoá mới.
    await expect(api('POST', '/projects', { slug: 'ops-2' })).rejects.toMatchObject({ status: 422 });
    const keys = [0, 1, 2, 3].map(i => headersOf(i)['Idempotency-Key']);
    expect(new Set(keys).size).toBe(4);
  });

  it('khoá giữ lại hết hạn sau 10 phút', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      fetchMock.mockRejectedValueOnce(new TypeError('failed to fetch'));
      await expect(api('POST', '/projects', { slug: 'ops' })).rejects.toMatchObject({ status: 0 });
      vi.advanceTimersByTime(10 * 60 * 1000 + 1);
      fetchMock.mockResolvedValueOnce(respond(201, {}));
      await api('POST', '/projects', { slug: 'ops' });
      expect(headersOf(1)['Idempotency-Key']).not.toBe(headersOf(0)['Idempotency-Key']);
    } finally {
      vi.useRealTimers();
    }
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

  it('getWithETag trả ETag của tài nguyên; PATCH gửi lại nó ở If-Match', async () => {
    fetchMock.mockResolvedValueOnce(new Response('{"id":"p1"}', { status: 200, headers: { ETag: '"abc"' } }));
    fetchMock.mockResolvedValueOnce(respond(200, { id: 'p1' }));
    const read = await getWithETag<{ id: string }>('/projects/p1');
    expect(read).toEqual({ data: { id: 'p1' }, etag: '"abc"' });
    await api('PATCH', '/projects/p1', { name: 'x' }, { ifMatch: read.etag });
    expect(headersOf(1)['If-Match']).toBe('"abc"');
    expect(fetchMock.mock.calls[1]![1].method).toBe('PATCH');
  });

  it('không có ETag thì không gửi If-Match', async () => {
    fetchMock.mockImplementation(async () => respond(200, {}));
    await expect(getWithETag('/budgets/b1')).resolves.toEqual({ data: {}, etag: null });
    await api('PATCH', '/budgets/b1', {}, { ifMatch: null });
    await api('PATCH', '/budgets/b1', {});
    expect(headersOf(1)['If-Match']).toBeUndefined();
    expect(headersOf(2)['If-Match']).toBeUndefined();
  });

  it('412 và 422 giữ nguyên mã và lỗi theo trường', async () => {
    fetchMock.mockResolvedValueOnce(respond(412, { title: 'Precondition Failed', code: 'precondition_failed' }));
    await expect(api('PATCH', '/projects/p1', {}, { ifMatch: '"old"' })).rejects.toMatchObject({ status: 412, code: 'precondition_failed' });
    fetchMock.mockResolvedValueOnce(respond(422, { title: 'Unprocessable', code: 'validation_failed', errors: [{ field: 'slug', message: 'đã tồn tại' }] }));
    const err = await api('POST', '/projects', {}).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 422, fields: [{ field: 'slug', message: 'đã tồn tại' }] });
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
