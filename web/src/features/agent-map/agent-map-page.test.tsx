import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

const state = vi.hoisted(() => ({ forbidden: false, noVersions: false, running: [] as unknown[] }));
const client = vi.hoisted(() => ({
  get: vi.fn(),
  soft: async <T,>(p: Promise<T>): Promise<T | null> => { try { return await p; } catch { return null; } },
}));
vi.mock('@/api/client', () => client);

import { ApiError } from '@/lib/api-error';
import { topology } from './fixtures';

const data = topology({ projects: 1, agents: 2, servers: 2, toolsPerServer: 2 });
const page = (rows: unknown[]) => ({ data: rows, next_cursor: null });

async function serve(path: string): Promise<unknown> {
  if (state.forbidden) throw new ApiError(403, { detail: 'forbidden' });
  if (path === '/organization') return { name: data.organization, slug: 'org', status: 'active' };
  if (path.startsWith('/projects?')) return page([data.agents[0]!.project]);
  if (path.startsWith('/mcp-servers')) return page(data.servers);
  if (path.startsWith('/mcp-tools')) return page(data.tools);
  if (path.startsWith('/approvals')) return page([{}, {}]);
  if (path.includes('/agents?')) return page(data.agents.map(a => a.agent));
  if (path.includes('/versions/') && state.noVersions) throw new ApiError(500, { detail: 'lỗi' });
  if (path.includes('/versions/')) return { config: data.agents.find(a => path.includes(a.agent.id))!.config };
  if (path.includes('/executions?status=running')) return page(state.running);
  if (path.includes('/executions?agent_id=')) return page([]);
  if (path.endsWith('/tasks?limit=100')) return page([{ id: 'task-1', agent_id: data.agents[0]!.agent.id }]);
  if (path.includes('/steps')) return page([{ tool_id: data.tools[0]!.id }]);
  // Audit chỉ org_admin đọc được.
  if (path.startsWith('/audit-logs')) throw new ApiError(403, { detail: 'forbidden' });
  throw new Error('đường dẫn không mong đợi: ' + path);
}

import AgentMapPage from './agent-map-page';

// jsdom không có các API đo kích thước mà thư viện đồ thị cần.
beforeAll(() => {
  class Observer { observe() { /* không đo */ } unobserve() { /* */ } disconnect() { /* */ } }
  vi.stubGlobal('ResizeObserver', Observer);
  vi.stubGlobal('DOMMatrixReadOnly', class { m22 = 1; constructor() { /* zoom 1 */ } });
});

beforeEach(() => { client.get.mockImplementation(serve); });
afterEach(() => { cleanup(); state.forbidden = false; state.noVersions = false; state.running = []; });

const show = () => render(
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter><AgentMapPage /></MemoryRouter>
  </QueryClientProvider>,
);

describe('Bản đồ Agent', () => {
  it('vẽ node từ dữ liệu của API: Control Plane, Agent, server, tool, hệ thống đích', async () => {
    show();
    expect(await screen.findByText('Tổ chức thử')).toBeTruthy();
    for (const name of ['Agent 0', 'Agent 1', 'Server 0', 'Server 1', 'host-0.example.internal:8443']) expect(screen.getByText(name)).toBeTruthy();
    // 4 tool (≤ 30) nên server mở sẵn.
    expect(screen.getAllByText('tool_0')).toHaveLength(2);
  });

  it('thu gọn rồi mở lại tool của một server bằng nút trên node', async () => {
    show();
    // Node trong jsdom không được đo kích thước nên thư viện để chúng ẩn: tìm nút theo chữ.
    const fold = (await screen.findAllByText('2 tool'))[0]!;
    expect(fold.getAttribute('aria-expanded')).toBe('true');
    fireEvent.click(fold);
    await waitFor(() => expect(screen.getAllByText('tool_0')).toHaveLength(1));
    fireEvent.click(screen.getAllByText('2 tool')[0]!);
    await waitFor(() => expect(screen.getAllByText('tool_0')).toHaveLength(2));
  });

  it('tìm một Agent rồi chọn: inspector hiện quyền gọi tool; đóng được', async () => {
    show();
    await screen.findByText('Agent 0');
    fireEvent.change(screen.getByPlaceholderText('Tìm node'), { target: { value: 'agent 0' } });
    fireEvent.click(screen.getByRole('button', { name: /Agent 0/ }));
    const panel = await screen.findByRole('complementary', { name: 'Chi tiết' });
    expect(within(panel).getByRole('heading', { name: 'Agent 0' })).toBeTruthy();
    expect(within(panel).getByText('1 cho phép · 1 từ chối')).toBeTruthy();
    expect(within(panel).getByText('Luôn cần duyệt')).toBeTruthy();
    expect(within(panel).getByText('Từ chối')).toBeTruthy();
    expect(within(panel).getByText(/vẫn do Gateway quyết định/)).toBeTruthy();
    // Đi tiếp sang một tool từ inspector: lịch sử gọi cần quyền đọc audit.
    fireEvent.click(within(panel).getAllByRole('button', { name: 'tool_0' })[0]!);
    await screen.findByRole('heading', { name: 'tool_0' });
    const tool = screen.getByRole('complementary', { name: 'Chi tiết' });
    expect(await within(tool).findByText(/chỉ quản trị viên organization đọc được/)).toBeTruthy();
    fireEvent.click(within(tool).getByRole('button', { name: 'Đóng' }));
    await waitFor(() => expect(screen.queryByRole('complementary', { name: 'Chi tiết' })).toBeNull());
  });

  it('lọc theo loại node và bỏ lọc bằng "Hiện tất cả"', async () => {
    show();
    await screen.findByText('Agent 0');
    fireEvent.click(screen.getByRole('button', { name: 'Bộ lọc' }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'Tool' }));
    await waitFor(() => expect(screen.queryByText('tool_0')).toBeNull());
    expect(screen.getByText('Server 0')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Hiện tất cả' }));
    await waitFor(() => expect(screen.getAllByText('tool_0')).toHaveLength(2));
  });

  it('theo dõi một lần chạy đang diễn ra: node của nó được đánh dấu', async () => {
    state.running = [{ id: '00000000-0000-4000-8009-0000000000aa', status: 'running' }];
    show();
    await screen.findByText('Agent 0');
    fireEvent.click(await screen.findByRole('button', { name: /000000aa/ }));
    await waitFor(() => expect(screen.getByText('Agent 0').closest('.map-node')?.getAttribute('data-live')).toBe('true'));
    expect(screen.getByText('Agent 1').closest('.map-node')?.getAttribute('data-live')).toBeNull();
  });

  it('version của Agent không đọc được: node ghi "không rõ quyền" và bản đồ cảnh báo, không báo là không có tool', async () => {
    state.noVersions = true;
    show();
    await screen.findByText('Agent 0');
    expect(screen.getAllByText('Không rõ quyền')).toHaveLength(2);
    expect(screen.queryByText('Không có tool')).toBeNull();
    expect(screen.getByText(/2 phần của bản đồ không đọc được/)).toBeTruthy();
  });

  it('không có quyền đọc: hiện lỗi thay vì trang trắng', async () => {
    state.forbidden = true;
    show();
    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Thử lại' })).toBeTruthy();
  });
});
