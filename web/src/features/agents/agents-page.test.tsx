import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const calls = vi.hoisted(() => ({ paths: [] as string[], forbidden: false, projects: true }));
const P1 = '00000000-0000-4000-8001-000000000001';
const P2 = '00000000-0000-4000-8001-000000000002';
const A1 = '00000000-0000-4000-8004-000000000001';

const client = vi.hoisted(() => ({
  get: vi.fn(),
  soft: async <T,>(p: Promise<T>): Promise<T | null> => { try { return await p; } catch { return null; } },
}));
vi.mock('@/api/client', () => client);

import { ApiError } from '@/lib/api-error';

const page = (data: unknown[]) => ({ data, next_cursor: null });

async function serve(path: string): Promise<unknown> {
  calls.paths.push(path);
  if (calls.forbidden) throw new ApiError(403, { detail: 'forbidden' });
  if (path.startsWith('/projects?')) return page(calls.projects ? [{ id: P1, name: 'Vận hành', environment: 'prod' }, { id: P2, name: 'Thử nghiệm', environment: 'non_prod' }] : []);
  if (path.includes('/agents?')) {
    if (path.includes('q=khong')) return page([]);
    return page([{ id: A1, project_id: P1, slug: 'devops', name: 'DevOps Agent', description: 'Đọc Docker', status: 'active', active_version: 2, latest_version: 3 }]);
  }
  if (path.includes('/versions/2')) return { config: { model: { policy: 'tiered', default_tier: 'standard' }, limits: { max_iterations: 8, max_tool_calls: 16, timeout_seconds: 300 }, tools: [{ server_id: 'srv-1', allow: ['list_containers'], deny: ['get_container_logs'], require_approval: 'always' }] } };
  if (path.startsWith('/mcp-servers')) return page([{ id: 'srv-1', name: 'Docker' }]);
  if (path.includes('/executions?agent_id=')) return page([]);
  throw new Error('đường dẫn không mong đợi: ' + path);
}

import AgentsPage from './agents-page';

beforeEach(() => { sessionStorage.clear(); client.get.mockImplementation(serve); });
afterEach(() => { cleanup(); calls.paths = []; calls.forbidden = false; calls.projects = true; });

const show = () => render(
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter><AgentsPage /></MemoryRouter>
  </QueryClientProvider>,
);

describe('Danh mục Agent', () => {
  it('liệt kê Agent của dự án đầu tiên; lọc trạng thái và tìm do máy chủ làm', async () => {
    show();
    expect(await screen.findByText('DevOps Agent')).toBeTruthy();
    expect(calls.paths).toContain(`/projects/${P1}/agents?limit=50&sort=name`);
    fireEvent.change(screen.getByLabelText('Trạng thái'), { target: { value: 'active' } });
    await waitFor(() => expect(calls.paths).toContain(`/projects/${P1}/agents?limit=50&sort=name&status=active`));
    fireEvent.change(screen.getByLabelText('Tìm'), { target: { value: 'khong co' } });
    expect(await screen.findByText('Không có Agent nào khớp bộ lọc.')).toBeTruthy();
    expect(calls.paths.at(-1)).toBe(`/projects/${P1}/agents?limit=50&sort=name&status=active&q=khong%20co`);
  });

  it('đổi dự án thì đọc Agent của dự án đó', async () => {
    show();
    await screen.findByText('DevOps Agent');
    fireEvent.change(screen.getByLabelText('Dự án'), { target: { value: P2 } });
    await waitFor(() => expect(calls.paths).toContain(`/projects/${P2}/agents?limit=50&sort=name`));
  });

  it('bấm một dòng mở inspector với cấu hình của version đang hoạt động', async () => {
    show();
    fireEvent.click(await screen.findByText('DevOps Agent'));
    const panel = await screen.findByRole('complementary', { name: 'Chi tiết' });
    expect(await within(panel).findByText('Docker')).toBeTruthy();
    expect(within(panel).getByText('Luôn cần duyệt')).toBeTruthy();
    expect(within(panel).getByText('list_containers')).toBeTruthy();
    expect(within(panel).getByText('get_container_logs')).toBeTruthy();
    expect(calls.paths).toContain(`/agents/${A1}/versions/2`);
    fireEvent.click(within(panel).getByRole('button', { name: 'Đóng' }));
    expect(screen.queryByRole('complementary', { name: 'Chi tiết' })).toBeNull();
  });

  it('chưa có dự án: trạng thái rỗng có bước tiếp theo', async () => {
    calls.projects = false;
    show();
    expect(await screen.findByRole('link', { name: 'Tạo dự án' })).toBeTruthy();
  });

  it('API từ chối: hiện lỗi thay vì trang trắng', async () => {
    calls.forbidden = true;
    show();
    expect(await screen.findByRole('alert')).toBeTruthy();
  });
});
