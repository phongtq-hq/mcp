import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({
  api: vi.fn<(method: string, path: string, body?: unknown) => Promise<unknown>>(),
  get: vi.fn<(path: string) => Promise<unknown>>(),
  soft: async <T,>(p: Promise<T>): Promise<T | null> => { try { return await p; } catch { return null; } },
}));
vi.mock('@/api/client', () => client);
// Luồng sự kiện: test tự phát sự kiện qua `emit`; luồng đóng khi test gọi `finish`.
const stream = vi.hoisted(() => ({ emit: null as null | (() => void), finish: null as null | ((end: string) => void), opened: 0 }));
vi.mock('@/api/events', () => ({
  streamExecutionEvents: (_id: string, onEvent: (e: unknown) => void, signal: AbortSignal) => new Promise<string>(resolve => {
    stream.opened++;
    stream.emit = () => onEvent({ event: 'step', data: null });
    stream.finish = resolve;
    signal.addEventListener('abort', () => resolve('closed'));
  }),
}));

import { ApiError } from '@/lib/api-error';
import ExecutionPage from './execution-page';
import ExecutionsTab from './executions-tab';

const P = '0193a1b2-0000-7000-8000-000000000001';
const A1 = '0193a1b2-0000-7000-8000-0000000000a1';
const X1 = '0193a1b2-0000-7000-8000-0000000000e1';
const T1 = '0193a1b2-0000-7000-8000-0000000000f1';
const usd = (amount: string) => ({ amount, currency: 'USD' });
const page = (data: unknown[], next: string | null = null) => ({ data, next_cursor: next });
const agent = { id: A1, project_id: P, slug: 'devops', name: 'DevOps Agent', status: 'active', active_version: 1 };
const execution = (over: Record<string, unknown> = {}) => ({
  id: X1, project_id: P, mode: 'single_agent', status: 'completed', estimated_cost: usd('0.00090000'),
  input: { message: 'Kiểm tra container' }, output: { message: 'Mọi thứ ổn.' }, created_at: '2026-11-19T09:47:44Z', ...over,
});
const task = { id: T1, execution_id: X1, agent_id: A1, agent_version: 3, goal: 'Kiểm tra container', status: 'completed', depth: 0, iterations: 2, tool_calls: 1, estimated_cost: usd('0.0009') };
const step = (over: Record<string, unknown>) => ({ id: 's' + String(over.step_seq), task_id: T1, status: 'succeeded', created_at: '2026-11-19T09:47:45Z', ...over });

const show = (path: string) => render(
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/projects/:projectId/executions" element={<ExecutionsTab />} />
        <Route path="/executions/:executionId" element={<ExecutionPage />} />
      </Routes>
    </MemoryRouter>
  </QueryClientProvider>,
);

beforeEach(() => { client.api.mockReset(); client.get.mockReset(); sessionStorage.clear(); stream.opened = 0; stream.emit = null; stream.finish = null; });
afterEach(cleanup);

describe('Tab lần chạy', () => {
  function serve() {
    client.get.mockImplementation(async (path: string) => (path.includes('/agents') ? page([agent]) : page([execution()])));
  }
  const listCalls = () => client.get.mock.calls.map(c => c[0]).filter(p => p.includes('/executions?'));

  it('hiện danh sách ngay; form chạy chỉ mở khi bấm "Chạy Agent"', async () => {
    serve();
    show(`/projects/${P}/executions`);
    expect(await screen.findByText('Kiểm tra container')).toBeTruthy();
    // Form có sẵn trong trang nhưng bị ẩn (thuộc tính hidden) cho tới khi bấm.
    const hiddenForm = () => screen.getByLabelText('Yêu cầu').closest('[hidden]');
    expect(hiddenForm()).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Chạy Agent' }));
    expect(hiddenForm()).toBeNull();
    // Đóng lại không làm mất nội dung đang gõ.
    fireEvent.change(screen.getByLabelText('Yêu cầu'), { target: { value: 'đang gõ dở' } });
    fireEvent.click(screen.getByRole('button', { name: 'Đóng' }));
    expect(hiddenForm()).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Chạy Agent' }));
    expect((screen.getByLabelText('Yêu cầu') as HTMLTextAreaElement).value).toBe('đang gõ dở');
    expect(client.api).not.toHaveBeenCalled();
  });

  it('lọc theo trạng thái và Agent do máy chủ làm (status, agent_id), không cắt ở trình duyệt', async () => {
    serve();
    show(`/projects/${P}/executions`);
    await screen.findByText('Kiểm tra container');
    expect(listCalls().at(-1)).toBe(`/projects/${P}/executions?limit=50`);
    fireEvent.change(screen.getByLabelText('Trạng thái'), { target: { value: 'failed' } });
    await waitFor(() => expect(listCalls().at(-1)).toBe(`/projects/${P}/executions?limit=50&status=failed`));
    fireEvent.change(await screen.findByLabelText('Agent'), { target: { value: A1 } });
    await waitFor(() => expect(listCalls().at(-1)).toBe(`/projects/${P}/executions?limit=50&status=failed&agent_id=${A1}`));
  });

  it('còn trang kế tiếp thì có nút tải thêm, gửi cursor của máy chủ', async () => {
    client.get.mockImplementation(async (path: string) => {
      if (path.includes('/agents')) return page([agent]);
      return path.includes('cursor=') ? page([execution({ id: X1.replace('e1', 'e2'), input: { message: 'Trang hai' } })]) : page([execution()], 'c2');
    });
    show(`/projects/${P}/executions`);
    fireEvent.click(await screen.findByRole('button', { name: 'Tải thêm' }));
    expect(await screen.findByText('Trang hai')).toBeTruthy();
    expect(listCalls().at(-1)).toBe(`/projects/${P}/executions?limit=50&cursor=c2`);
  });

  it('không có dòng nào khớp bộ lọc: nói rõ là do bộ lọc', async () => {
    client.get.mockImplementation(async (path: string) => (path.includes('/agents') ? page([agent]) : page([])));
    show(`/projects/${P}/executions`);
    expect(await screen.findByText('Chưa có lần chạy nào.')).toBeTruthy();
    // Trạng thái rỗng nói bước tiếp theo và có luôn nút làm bước đó.
    expect(screen.getByText(/Mỗi lần chạy gọi model và có tính chi phí/)).toBeTruthy();
    expect(screen.getAllByRole('button', { name: 'Chạy Agent' })).toHaveLength(2);
    fireEvent.change(screen.getByLabelText('Trạng thái'), { target: { value: 'failed' } });
    expect(await screen.findByText('Không có lần chạy nào khớp bộ lọc.')).toBeTruthy();
  });
});

// Tên tool hiện ở cả sơ đồ luồng lẫn danh sách bước: tìm trong danh sách bước.
const inSteps = async (text: string) => within(await waitFor(() => { const el = document.querySelector<HTMLElement>('.steps'); if (!el) throw new Error('chưa có danh sách bước'); return el; })).getByText(text);

describe('Chi tiết lần chạy', () => {
  function serve(content: () => Promise<unknown>) {
    client.get.mockImplementation(async (path: string) => {
      if (path === '/executions/' + X1) return execution();
      if (path.startsWith(`/executions/${X1}/tasks`)) return page([task]);
      if (path === '/agents/' + A1) return agent;
      if (path.includes('include_content=true')) return content();
      return page([
        step({ step_seq: 1, kind: 'llm_call', routing: '{"model_used":"model-x"}' }),
        step({ step_seq: 2, kind: 'tool_call', tool_name: 'docker.list_containers' }),
      ]);
    });
  }

  it('nêu tên Agent, version và model ở đầu trang thay cho mã', async () => {
    serve(async () => page([]));
    show('/executions/' + X1);
    await screen.findByText('v3 · model-x');
    expect([...document.querySelectorAll('.stat .value')].map(el => el.textContent)).toContain('DevOps Agent');
    expect(document.title).toContain('Lần chạy');
  });

  it('nội dung các bước chỉ tải khi bấm xem; hiện request / response dạng văn bản', async () => {
    serve(async () => page([
      step({ step_seq: 1, kind: 'llm_call', request: '{"model":"model-x"}', response: { stop_reason: 'tool_use' } }),
      step({ step_seq: 2, kind: 'tool_call', tool_name: 'docker.list_containers', request: '{"arguments":{"all":true}}', response: '<b>không phải HTML</b>' }),
    ]));
    show('/executions/' + X1);
    await inSteps('docker.list_containers');
    expect(client.get.mock.calls.some(c => c[0].includes('include_content'))).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Xem nội dung các bước' }));
    expect(await screen.findByText(/"all": true/)).toBeTruthy();
    expect(screen.getByText(/"stop_reason": "tool_use"/)).toBeTruthy();
    // Chuỗi không phải JSON hiện nguyên văn dạng text, không thành phần tử HTML.
    expect(screen.getByText('<b>không phải HTML</b>')).toBeTruthy();
    expect(document.querySelector('.steps b')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Ẩn nội dung các bước' }));
    expect(screen.queryByText(/"all": true/)).toBeNull();
  });

  it('thiếu quyền đọc nội dung (403): báo thiếu quyền, các bước vẫn hiện', async () => {
    serve(async () => { throw new ApiError(403, { detail: 'thiếu execution:read_content' }); });
    show('/executions/' + X1);
    await inSteps('docker.list_containers');
    fireEvent.click(screen.getByRole('button', { name: 'Xem nội dung các bước' }));
    expect((await screen.findByRole('alert')).textContent).toContain('không có quyền');
    expect(await inSteps('docker.list_containers')).toBeTruthy();
  });

  it('không tải được lần chạy: có nút thử lại và đường về danh sách dự án', async () => {
    client.get.mockRejectedValue(new ApiError(404, { trace_id: 'abcdef0123456789' }));
    show('/executions/' + X1);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Không tìm thấy');
    expect(within(alert).getByText('abcdef0123456789')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Dự án' })).toBeTruthy();
    const calls = client.get.mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: 'Thử lại' }));
    await waitFor(() => expect(client.get.mock.calls.length).toBeGreaterThan(calls));
  });
});

describe('Sơ đồ luồng của lần chạy', () => {
  const node = (name: string) => screen.getByText(name).closest('.flow-node') as HTMLElement;

  it('lần chạy đã xong: mỗi node nêu trạng thái bằng chữ, số lượt và thời gian từ các bước thật; không mở luồng sự kiện', async () => {
    client.get.mockImplementation(async (path: string) => {
      if (path === '/executions/' + X1) return execution();
      if (path.startsWith(`/executions/${X1}/tasks`)) return page([task]);
      if (path === '/agents/' + A1) return agent;
      return page([
        step({ step_seq: 1, kind: 'llm_call', duration_ms: 1200, routing: '{"model_used":"model-x"}' }),
        step({ step_seq: 2, kind: 'tool_call', tool_name: 'docker.list_containers', duration_ms: 180 }),
        step({ step_seq: 3, kind: 'llm_call', duration_ms: 3800 }),
      ]);
    });
    show('/executions/' + X1);
    const flow = await screen.findByRole('region', { name: 'Luồng của lần chạy' });
    expect(within(flow).getByText('2 lượt · 5.000 ms')).toBeTruthy();
    expect(within(flow).getByText('1 lượt · 180 ms')).toBeTruthy();
    expect(node('Kết quả').dataset.state).toBe('done');
    expect(within(node('Kết quả')).getByText('Xong')).toBeTruthy();
    expect(within(flow).getByRole('button', { name: 'Phát lại' })).toBeTruthy();
    expect(stream.opened).toBe(0);
  });

  it('lần chạy đang chạy: node của bước đang mở được làm nổi; có sự kiện thì tải lại ngay, không chờ nhịp tự làm mới', async () => {
    let steps = [step({ step_seq: 1, kind: 'llm_call', status: 'started' })];
    client.get.mockImplementation(async (path: string) => {
      if (path === '/executions/' + X1) return execution({ status: 'running', output: null });
      if (path.startsWith(`/executions/${X1}/tasks`)) return page([{ ...task, status: 'running' }]);
      if (path === '/agents/' + A1) return agent;
      return page(steps);
    });
    show('/executions/' + X1);
    await screen.findByRole('region', { name: 'Luồng của lần chạy' });
    expect(node('Model').dataset.state).toBe('active');
    expect(document.querySelector('.flow-edge[data-active]')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Phát lại' })).toBeNull();
    await waitFor(() => expect(stream.opened).toBe(1));
    steps = [step({ step_seq: 1, kind: 'llm_call' }), step({ step_seq: 2, kind: 'tool_call', status: 'started', tool_name: 'docker.list_containers' })];
    stream.emit!();
    await waitFor(() => expect(node('Gateway + tool').dataset.state).toBe('active'));
    expect(node('Model').dataset.state).toBe('done');
    // Bước vừa xuất hiện được đánh dấu là mới.
    expect(document.querySelectorAll('.steps li.row-new')).toHaveLength(1);
  });
});
