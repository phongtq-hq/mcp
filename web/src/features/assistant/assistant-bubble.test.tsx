import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({
  api: vi.fn<(method: string, path: string, body?: unknown) => Promise<unknown>>(),
  get: vi.fn<(path: string) => Promise<unknown>>(),
}));
vi.mock('@/api/client', () => client);

import { ApiError } from '@/lib/api-error';
import AssistantBubble from './assistant-bubble';

const P = '0193a1b2-0000-7000-8000-000000000001';
const A = '0193a1b2-0000-7000-8000-000000000002';
const X1 = '0193a1b2-0000-7000-8000-0000000000a1';
const X2 = '0193a1b2-0000-7000-8000-0000000000a2';

const org = (settings: unknown) => ({ id: P, slug: 'acme', name: 'Acme', status: 'active', settings });
const run = (id: string, status: string, message?: string) => ({
  id, project_id: P, mode: 'single_agent', status, estimated_cost: { amount: '0', currency: 'USD' },
  created_at: new Date().toISOString(), output: message === undefined ? null : { message },
});

// answers: trả lời của từng lần chạy; lần chạy chưa có trong đây còn đang chạy.
function serve(settings: unknown, answers: Record<string, string>) {
  client.get.mockImplementation(async (path: string) => {
    if (path === '/organization') return org(settings);
    const id = path.replace('/executions/', '');
    return id in answers ? run(id, 'completed', answers[id]) : run(id, 'running');
  });
}

const show = (path = `/projects/${P}/settings`) => render(
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter initialEntries={[path]}><AssistantBubble /></MemoryRouter>
  </QueryClientProvider>,
);

const configured = { console_assistant: { project_id: P, agent_id: A } };
const ask = (text: string) => {
  fireEvent.change(screen.getByLabelText('Câu hỏi'), { target: { value: text } });
  fireEvent.click(screen.getByRole('button', { name: 'Gửi' }));
};
const sentMessage = (n: number) => (client.api.mock.calls[n]![2] as { input: { message: string } }).input.message;

beforeEach(() => { client.api.mockReset(); client.get.mockReset(); });
afterEach(cleanup);

describe('Bong bóng trợ lý', () => {
  it('organization chưa cấu hình trợ lý, hoặc không đọc được organization: không có bong bóng', async () => {
    serve({}, {});
    const first = show();
    await waitFor(() => expect(client.get).toHaveBeenCalledWith('/organization'));
    expect(screen.queryByRole('button', { name: 'Trợ lý' })).toBeNull();
    first.unmount();
    client.get.mockRejectedValue(new ApiError(403, null));
    show();
    await waitFor(() => expect(client.get).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole('button', { name: 'Trợ lý' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('gửi câu hỏi thành một lần chạy single_agent của Agent trợ lý, kèm loại màn hình (không kèm định danh), rồi hiện trả lời', async () => {
    serve(configured, { [X1]: 'Bấm Lưu ở cuối trang.' });
    client.api.mockResolvedValue(run(X1, 'queued'));
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'Trợ lý' }));
    ask('Lưu cài đặt thế nào?');
    await waitFor(() => expect(client.api).toHaveBeenCalledTimes(1));
    const [method, path, body] = client.api.mock.calls[0]!;
    expect([method, path]).toEqual(['POST', `/projects/${P}/executions`]);
    expect(body).toMatchObject({ mode: 'single_agent', agent_id: A });
    expect(sentMessage(0)).toContain('path: /projects/:id/settings');
    expect(sentMessage(0)).toContain('Lưu cài đặt thế nào?');
    expect(sentMessage(0)).not.toContain(P);
    expect(await screen.findByText('Bấm Lưu ở cuối trang.')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Xem lần chạy' }).getAttribute('href')).toBe('/executions/' + X1);
    expect((screen.getByLabelText('Câu hỏi') as HTMLTextAreaElement).value).toBe('');
  });

  it('trong lúc chờ trả lời không gửi được câu kế tiếp; câu sau mang lượt trước làm ngữ cảnh', async () => {
    const answers: Record<string, string> = {};
    serve(configured, answers);
    client.api.mockResolvedValueOnce(run(X1, 'queued')).mockResolvedValueOnce(run(X2, 'queued'));
    show('/approvals');
    fireEvent.click(await screen.findByRole('button', { name: 'Trợ lý' }));
    ask('Approval là gì?');
    expect(await screen.findByText('Đang trả lời…')).toBeTruthy();
    await waitFor(() => expect((screen.getByRole('button', { name: 'Gửi' }) as HTMLButtonElement).disabled).toBe(true));
    answers[X1] = 'Một yêu cầu chờ người duyệt.';
    expect(await screen.findByText('Một yêu cầu chờ người duyệt.', undefined, { timeout: 4000 })).toBeTruthy();
    ask('Ai duyệt được?');
    await waitFor(() => expect(client.api).toHaveBeenCalledTimes(2));
    expect(sentMessage(1)).toContain('user: Approval là gì?\nassistant: Một yêu cầu chờ người duyệt.');
  });

  it('API từ chối tạo lần chạy: hiện lỗi thật, giữ câu hỏi đang gõ', async () => {
    serve(configured, {});
    client.api.mockRejectedValue(new ApiError(402, { detail: 'budget đã chạm ngưỡng' }));
    show();
    fireEvent.click(await screen.findByRole('button', { name: 'Trợ lý' }));
    ask('Xin chào');
    expect((await screen.findByRole('alert')).textContent).toContain('budget đã chạm ngưỡng');
    expect((screen.getByLabelText('Câu hỏi') as HTMLTextAreaElement).value).toBe('Xin chào');
  });

  it('thu gọn rồi mở lại vẫn còn cuộc trò chuyện', async () => {
    serve(configured, { [X1]: 'Trả lời.' });
    client.api.mockResolvedValue(run(X1, 'queued'));
    show();
    const toggle = await screen.findByRole('button', { name: 'Trợ lý' });
    fireEvent.click(toggle);
    ask('Câu một');
    expect(await screen.findByText('Trả lời.')).toBeTruthy();
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(toggle);
    expect(screen.getByText('Trả lời.')).toBeTruthy();
    expect(client.api).toHaveBeenCalledTimes(1);
  });
});
