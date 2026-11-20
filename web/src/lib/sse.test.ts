import { describe, expect, it } from 'vitest';
import { MAX_SSE_EVENT_CHARS, newSSEParser, SSEOverflowError, type SSEEvent } from './sse';

function collect(chunks: string[]): SSEEvent[] {
  const out: SSEEvent[] = [];
  const push = newSSEParser(e => out.push(e));
  for (const c of chunks) push(c);
  return out;
}

describe('newSSEParser', () => {
  it('đọc sự kiện đúng dạng máy chủ gửi', () => {
    expect(collect(['id: 3\nevent: step\ndata: {"task_id":"t1","step_seq":2}\n\n'])).toEqual([
      { event: 'step', data: '{"task_id":"t1","step_seq":2}', id: '3' },
    ]);
  });

  it('sự kiện bị cắt ở bất kỳ đâu giữa các mẩu vẫn ra đúng một lần', () => {
    const wire = 'event: task\ndata: {"id":"a"}\n\nevent: end\ndata: {}\n\n';
    for (let cut = 1; cut < wire.length; cut++) {
      const got = collect([wire.slice(0, cut), wire.slice(cut)]);
      expect(got.map(e => e.event)).toEqual(['task', 'end']);
      expect(got[0]!.data).toBe('{"id":"a"}');
    }
  });

  it('nhận cả \\r\\n và \\r, kể cả khi \\r\\n bị cắt đôi', () => {
    expect(collect(['event: a\r\ndata: 1\r\n\r\n']).map(e => [e.event, e.data])).toEqual([['a', '1']]);
    expect(collect(['event: a\r', '\ndata: 1\r', '\n\r', '\n']).map(e => [e.event, e.data])).toEqual([['a', '1']]);
    // "\r" cuối mẩu được giữ lại (có thể là nửa đầu của "\r\n") tới khi có mẩu kế tiếp.
    expect(collect(['data: x\r\rdata: y\r\r']).map(e => e.data)).toEqual(['x']);
    expect(collect(['data: x\r\rdata: y\r\r', ': giữ kết nối\n']).map(e => e.data)).toEqual(['x', 'y']);
  });

  it('nhiều dòng data nối bằng xuống dòng; không có event thì là message', () => {
    expect(collect(['data: a\ndata: b\n\n'])).toEqual([{ event: 'message', data: 'a\nb', id: null }]);
  });

  it('bỏ dòng chú thích và trường lạ; dòng trống thừa không sinh sự kiện', () => {
    expect(collect([': giữ kết nối\n\nretry: 1000\nfoo: bar\n\n\n\nevent: x\ndata: 1\n\n']).map(e => e.event)).toEqual(['x']);
  });

  it('sự kiện không có dòng data bị bỏ (theo chuẩn)', () => {
    expect(collect(['event: ping\n\nevent: x\ndata: 1\n\n']).map(e => e.event)).toEqual(['x']);
  });

  it('sự kiện chưa kết thúc bằng dòng trống thì chưa được phát', () => {
    expect(collect(['event: step\ndata: {}\n'])).toEqual([]);
  });

  it('từ chối sự kiện vượt trần thay vì gom mãi vào bộ nhớ', () => {
    const push = newSSEParser(() => {});
    expect(() => push('data: ' + 'x'.repeat(MAX_SSE_EVENT_CHARS + 1))).toThrow(SSEOverflowError);
    const many = newSSEParser(() => {});
    const line = 'data: ' + 'y'.repeat(1024) + '\n';
    expect(() => { for (let i = 0; i < 100; i++) many(line); }).toThrow(SSEOverflowError);
  });
});
