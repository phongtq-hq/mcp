import { beforeEach, describe, expect, it, vi } from 'vitest';

const auth = vi.hoisted(() => ({
  accessToken: vi.fn<() => Promise<string | null>>(),
  apiBase: vi.fn(() => '/v1'),
  expireSession: vi.fn(),
}));
vi.mock('@/auth/oidc', () => auth);

import { ApiError } from '@/lib/api-error';
import { streamExecutionEvents, type ExecutionEvent } from './events';

const fetchMock = vi.fn<(url: string, init: RequestInit) => Promise<Response>>();

function stream(chunks: string[], keepOpen = false): Response {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(encoder.encode(c));
      if (!keepOpen) controller.close();
    },
  });
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
}

beforeEach(() => {
  fetchMock.mockReset();
  auth.accessToken.mockReset().mockResolvedValue('token-1');
  auth.expireSession.mockReset();
  vi.stubGlobal('fetch', fetchMock);
});

describe('streamExecutionEvents', () => {
  it('gửi token ở header (không ở URL) và phát từng sự kiện tới khi end', async () => {
    fetchMock.mockResolvedValue(stream(['id: 1\nevent: task\ndata: {"task_id":"t1","status":"running"}\n\n', 'id: 2\nevent: step\ndata: {"task_id":"t1","step_seq":1}\n\nevent: end\ndata: {}\n\n']));
    const got: ExecutionEvent[] = [];
    const end = await streamExecutionEvents('e 1', e => got.push(e), new AbortController().signal);
    expect(end).toBe('end');
    expect(got).toEqual([
      { event: 'task', data: { task_id: 't1', status: 'running' } },
      { event: 'step', data: { task_id: 't1', step_seq: 1 } },
    ]);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe('/v1/executions/e%201/events');
    expect(url).not.toContain('token');
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer token-1');
  });

  it('reconnect và kết nối đứt được phân biệt', async () => {
    fetchMock.mockResolvedValueOnce(stream(['event: reconnect\ndata: {}\n\n']));
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).resolves.toBe('reconnect');
    fetchMock.mockResolvedValueOnce(stream(['event: task\ndata: {}\n\n']));
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).resolves.toBe('closed');
  });

  it('máy chủ báo lỗi trên luồng (mất quyền, lỗi nội bộ): kết cục error, không phải "kết nối đứt"', async () => {
    fetchMock.mockResolvedValueOnce(stream(['event: task\ndata: {"task_id":"t1"}\n\n', 'event: error\ndata: {"code":"permission_denied"}\n\n']));
    const got: ExecutionEvent[] = [];
    await expect(streamExecutionEvents('e1', e => got.push(e), new AbortController().signal)).resolves.toBe('error');
    expect(got).toEqual([{ event: 'task', data: { task_id: 't1' } }]);
  });

  it('sự kiện đứng sau sự kiện kết thúc trong cùng một mẩu bị bỏ, và không ghi đè kết cục', async () => {
    fetchMock.mockResolvedValueOnce(stream(['event: end\ndata: {}\n\nevent: step\ndata: {"x":1}\n\nevent: reconnect\ndata: {}\n\n']));
    const got: ExecutionEvent[] = [];
    await expect(streamExecutionEvents('e1', e => got.push(e), new AbortController().signal)).resolves.toBe('end');
    expect(got).toEqual([]);
  });

  it('dừng đọc ngay khi gặp end, kể cả khi máy chủ chưa đóng luồng', async () => {
    fetchMock.mockResolvedValue(stream(['event: end\ndata: {}\n\n'], true));
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).resolves.toBe('end');
  });

  it('thân sự kiện không phải JSON cho data null, không ném lỗi', async () => {
    fetchMock.mockResolvedValue(stream(['event: task\ndata: <html>\n\n']));
    const got: ExecutionEvent[] = [];
    await streamExecutionEvents('e1', e => got.push(e), new AbortController().signal);
    expect(got).toEqual([{ event: 'task', data: null }]);
  });

  it('lỗi HTTP thành ApiError; 401 kết thúc phiên; không có token thì không gọi mạng', async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ title: 'Too Many Requests', code: 'rate_limited' }), { status: 429 }));
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).rejects.toMatchObject({ status: 429, code: 'rate_limited' });
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 401 }));
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).rejects.toMatchObject({ status: 401 });
    expect(auth.expireSession).toHaveBeenCalledTimes(1);
    auth.accessToken.mockResolvedValue(null);
    fetchMock.mockClear();
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).rejects.toBeInstanceOf(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('huỷ (rời màn hình) kết thúc êm, không ném lỗi', async () => {
    const abort = new AbortController();
    fetchMock.mockImplementation((_url, init) => new Promise((_resolve, reject) => {
      // Như fetch thật: tín hiệu đã huỷ từ trước thì từ chối ngay.
      const fail = () => reject(new DOMException('aborted', 'AbortError'));
      if (init.signal!.aborted) fail(); else init.signal!.addEventListener('abort', fail);
    }));
    const done = streamExecutionEvents('e1', () => {}, abort.signal);
    abort.abort();
    await expect(done).resolves.toBe('closed');
  });

  it('không tới được máy chủ: status 0', async () => {
    fetchMock.mockRejectedValue(new TypeError('failed to fetch'));
    await expect(streamExecutionEvents('e1', () => {}, new AbortController().signal)).rejects.toMatchObject({ status: 0 });
  });
});
