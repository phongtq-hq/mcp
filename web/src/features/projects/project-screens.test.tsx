import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({
  api: vi.fn<(method: string, path: string, body?: unknown, options?: unknown) => Promise<unknown>>(),
  get: vi.fn<(path: string) => Promise<unknown>>(),
  getWithETag: vi.fn<(path: string) => Promise<unknown>>(),
  soft: vi.fn((p: Promise<unknown>) => p.catch(() => null)),
}));
vi.mock('@/api/client', () => client);

import { ApiError } from '@/lib/api-error';
import ProjectNewPage from './project-new-page';
import ProjectSettingsTab from './project-settings-tab';

const P = '0193a1b2-0000-7000-8000-0000000000aa';
const AGENT = '0193a1b2-0000-7000-8000-0000000000bb';

function Where() {
  return <div data-testid="where">{useLocation().pathname}</div>;
}

function show(path: string) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/projects/new" element={<ProjectNewPage />} />
          <Route path="/projects/:projectId/settings" element={<ProjectSettingsTab />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const project = (over: Record<string, unknown> = {}) => ({
  id: P, slug: 'ops', name: 'Ops', environment: 'non_prod', created_at: '2026-11-19T00:00:00Z', updated_at: '2026-11-19T00:00:00Z',
  settings: { model_routing: { tiers: {} }, orchestration: { planned_enabled: false } }, ...over,
});

beforeEach(() => {
  for (const fn of [client.api, client.get, client.getWithETag]) fn.mockReset();
});
afterEach(cleanup);

describe('Tạo dự án', () => {
  it('gửi POST /projects với giá trị đã nhập rồi mở dự án vừa tạo', async () => {
    client.api.mockResolvedValue(project());
    show('/projects/new');
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: ' Ops ' } });
    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'ops' } });
    fireEvent.change(screen.getByLabelText('Môi trường'), { target: { value: 'non_prod' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/projects/' + P));
    expect(client.api).toHaveBeenCalledTimes(1);
    expect(client.api).toHaveBeenCalledWith('POST', '/projects', { name: 'Ops', slug: 'ops', environment: 'non_prod' });
  });

  it('slug trùng (409) và dữ liệu sai (422): hiện nguyên thông báo của API, không rời trang, giữ nội dung đã nhập', async () => {
    client.api.mockRejectedValueOnce(new ApiError(409, { detail: 'slug đã được dùng trong organization này', code: 'slug_taken' }));
    show('/projects/new');
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: 'Ops' } });
    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'ops' } });
    fireEvent.change(screen.getByLabelText('Môi trường'), { target: { value: 'prod' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    expect((await screen.findByRole('alert')).textContent).toContain('slug đã được dùng trong organization này');
    expect(screen.queryByText(/đã bị thay đổi ở nơi khác/)).toBeNull();
    expect(screen.queryByTestId('where')).toBeNull();
    expect(screen.getByLabelText('Slug')).toHaveProperty('value', 'ops');

    client.api.mockRejectedValueOnce(new ApiError(422, { detail: 'slug chỉ gồm chữ thường, số và dấu gạch ngang', code: 'validation_failed' }));
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('slug chỉ gồm chữ thường'));
    expect(client.api).toHaveBeenCalledTimes(2);
  });

  it('chưa chọn môi trường thì không gọi API', () => {
    show('/projects/new');
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: 'Ops' } });
    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    expect(client.api).not.toHaveBeenCalled();
  });

  it('Huỷ quay về danh sách, không gọi API', () => {
    show('/projects/new');
    fireEvent.click(screen.getByRole('button', { name: 'Huỷ' }));
    expect(screen.getByTestId('where').textContent).toBe('/projects');
    expect(client.api).not.toHaveBeenCalled();
  });
});

describe('Cài đặt dự án', () => {
  const agentsPage = { data: [{ id: AGENT, name: 'Planner' }], next_cursor: null };
  // Mỗi lần đọc trả một ETag khác, như máy chủ thật sau mỗi lần ghi.
  function loaded(over: Record<string, unknown> = {}, agents: unknown = agentsPage) {
    let n = 0;
    client.getWithETag.mockImplementation(async () => ({ data: project(over), etag: `"e${++n}"` }));
    if (agents instanceof Error) client.get.mockRejectedValue(agents); else client.get.mockResolvedValue(agents);
  }
  const section = (name: string) => screen.getByRole('region', { name });
  const saveIn = (name: string) => fireEvent.click(section(name).querySelector('button[type=submit]')!);
  const GENERAL = 'Thông tin chung';
  const ORCH = 'Điều phối (mode planned)';
  const JSON_SECTION = 'Cài đặt nâng cao (JSON)';

  it('không đọc được dự án (403): hiện lỗi, không hiện form', async () => {
    client.getWithETag.mockRejectedValue(new ApiError(403, { detail: 'thiếu quyền project:read' }));
    client.get.mockResolvedValue({ data: [], next_cursor: null });
    show(`/projects/${P}/settings`);
    expect((await screen.findByRole('alert')).textContent).toContain('không có quyền');
    expect(screen.queryByRole('button', { name: 'Lưu' })).toBeNull();
  });

  it('đổi tên gửi PATCH kèm ETag; "Đã lưu." còn hiện sau khi trang đọc lại; lần lưu sau dùng ETag mới', async () => {
    loaded();
    client.api.mockResolvedValue(project({ name: 'Vận hành' }));
    show(`/projects/${P}/settings`);
    fireEvent.change(await screen.findByLabelText('Tên'), { target: { value: 'Vận hành' } });
    saveIn(GENERAL);
    await waitFor(() => expect(client.api).toHaveBeenCalledWith('PATCH', '/projects/' + P, { name: 'Vận hành' }, { ifMatch: '"e1"' }));
    await waitFor(() => expect(client.getWithETag).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('Đã lưu.')).toBeTruthy();
    expect(section(GENERAL).textContent).toContain('Đã lưu.');

    // Mục khác lưu ngay sau đó phải gửi ETag của bản vừa đọc lại, không phải bản cũ.
    await waitFor(() => expect(section(ORCH).querySelector('fieldset')!.disabled).toBe(false));
    fireEvent.click(screen.getByLabelText('Cho phép lần chạy theo kế hoạch (mode planned)'));
    saveIn(ORCH);
    await waitFor(() => expect(client.api).toHaveBeenCalledTimes(2));
    expect(client.api.mock.calls[1]![3]).toEqual({ ifMatch: '"e2"' });
  });

  it('một mục đang sửa dở khoá các mục khác; bỏ thay đổi thì mở lại và trả về giá trị đã lưu', async () => {
    loaded();
    show(`/projects/${P}/settings`);
    fireEvent.change(await screen.findByLabelText('Tên'), { target: { value: 'đang gõ dở' } });
    expect(section(ORCH).querySelector('fieldset')!.disabled).toBe(true);
    expect(section(JSON_SECTION).querySelector('fieldset')!.disabled).toBe(true);
    expect(section(GENERAL).querySelector('fieldset')!.disabled).toBe(false);
    expect(section(ORCH).textContent).toContain('đang có thay đổi chưa lưu');

    fireEvent.click(screen.getByRole('button', { name: 'Bỏ thay đổi' }));
    expect(screen.getByLabelText('Tên')).toHaveProperty('value', 'Ops');
    expect(section(ORCH).querySelector('fieldset')!.disabled).toBe(false);
    expect(client.api).not.toHaveBeenCalled();
  });

  it('lưu điều phối giữ nguyên các khoá settings khác và khoá lạ trong orchestration', async () => {
    loaded({ settings: { model_routing: { tiers: {} }, orchestration: { planned_enabled: false, khoa_tuong_lai: 7 } } });
    client.api.mockResolvedValue(project());
    show(`/projects/${P}/settings`);
    fireEvent.click(await screen.findByLabelText('Cho phép lần chạy theo kế hoạch (mode planned)'));
    fireEvent.change(screen.getByLabelText('Agent lập kế hoạch'), { target: { value: AGENT } });
    fireEvent.change(screen.getByLabelText('Số task chạy đồng thời'), { target: { value: '6' } });
    saveIn(ORCH);
    await waitFor(() => expect(client.api).toHaveBeenCalled());
    expect(client.api).toHaveBeenCalledWith('PATCH', '/projects/' + P, {
      settings: { model_routing: { tiers: {} }, orchestration: { planned_enabled: true, khoa_tuong_lai: 7, planner_agent_id: AGENT, max_parallel_tasks: 6 } },
    }, { ifMatch: '"e1"' });
  });

  it('bỏ chọn Agent lập kế hoạch và xoá số task: hai khoá đó không còn trong thân gửi đi', async () => {
    loaded({ settings: { orchestration: { planned_enabled: true, planner_agent_id: AGENT, max_parallel_tasks: 2 } } });
    client.api.mockResolvedValue(project());
    show(`/projects/${P}/settings`);
    fireEvent.change(await screen.findByLabelText('Agent lập kế hoạch'), { target: { value: '' } });
    fireEvent.change(screen.getByLabelText('Số task chạy đồng thời'), { target: { value: '' } });
    saveIn(ORCH);
    await waitFor(() => expect(client.api).toHaveBeenCalled());
    expect(client.api.mock.calls[0]![2]).toEqual({ settings: { orchestration: { planned_enabled: true } } });
  });

  it('không đọc được danh sách Agent: Agent lập kế hoạch đang lưu vẫn được giữ khi lưu', async () => {
    loaded({ settings: { orchestration: { planned_enabled: true, planner_agent_id: AGENT } } }, new ApiError(403, null));
    client.api.mockResolvedValue(project());
    show(`/projects/${P}/settings`);
    expect(await screen.findByText(/Không đọc được danh sách Agent/)).toBeTruthy();
    saveIn(ORCH);
    await waitFor(() => expect(client.api).toHaveBeenCalled());
    expect(client.api.mock.calls[0]![2]).toEqual({ settings: { orchestration: { planned_enabled: true, planner_agent_id: AGENT } } });
  });

  it('lưu bị từ chối: 412 báo xung đột, 403 và 409 hiện nguyên thông báo của API; không báo đã lưu', async () => {
    loaded();
    show(`/projects/${P}/settings`);
    await screen.findByLabelText('Tên');
    client.api.mockRejectedValueOnce(new ApiError(412, { detail: 'tài nguyên đã thay đổi' }));
    saveIn(GENERAL);
    expect(await screen.findByText(/đã bị thay đổi ở nơi khác/)).toBeTruthy();
    client.api.mockRejectedValueOnce(new ApiError(403, { detail: 'thiếu quyền project:update' }));
    saveIn(GENERAL);
    await waitFor(() => expect(section(GENERAL).textContent).toContain('thiếu quyền project:update'));
    client.api.mockRejectedValueOnce(new ApiError(409, { detail: 'project đã được lưu trữ', code: 'project_archived' }));
    saveIn(GENERAL);
    await waitFor(() => expect(section(GENERAL).textContent).toContain('project đã được lưu trữ'));
    expect(screen.queryByText('Đã lưu.')).toBeNull();
    expect(client.getWithETag).toHaveBeenCalledTimes(1);
  });

  it('máy chủ không gửi ETag: từ chối lưu thay vì ghi vô điều kiện', async () => {
    client.getWithETag.mockResolvedValue({ data: project(), etag: null });
    client.get.mockResolvedValue(agentsPage);
    show(`/projects/${P}/settings`);
    await screen.findByLabelText('Tên');
    saveIn(GENERAL);
    expect(await screen.findByText(/máy chủ không gửi phiên bản/)).toBeTruthy();
    expect(client.api).not.toHaveBeenCalled();
  });

  it('JSON settings rỗng, sai cú pháp hoặc không phải object: nút Lưu bị khoá và có lời giải thích', async () => {
    loaded();
    show(`/projects/${P}/settings`);
    const area = await screen.findByLabelText('Settings (JSON)');
    const button = section(JSON_SECTION).querySelector<HTMLButtonElement>('button[type=submit]')!;
    expect(button.disabled).toBe(false);
    for (const bad of ['[1,2]', '{"a":', '']) {
      fireEvent.change(area, { target: { value: bad } });
      expect(button.disabled).toBe(true);
      expect(section(JSON_SECTION).textContent).toContain('Settings phải là một object JSON hợp lệ.');
    }
    fireEvent.change(area, { target: { value: '{"a":1}' } });
    expect(button.disabled).toBe(false);
    fireEvent.click(button);
    await waitFor(() => expect(client.api).toHaveBeenCalledWith('PATCH', '/projects/' + P, { settings: { a: 1 } }, { ifMatch: '"e1"' }));
  });

  it('lưu trữ: huỷ xác nhận thì không gọi DELETE; xác nhận thì gọi và về danh sách', async () => {
    loaded();
    client.api.mockResolvedValue(null);
    show(`/projects/${P}/settings`);
    fireEvent.click(await screen.findByRole('button', { name: 'Lưu trữ dự án' }));
    // Trong lúc hỏi, nút bị khoá: không mở được hộp thứ hai.
    expect(screen.getByRole('button', { name: 'Lưu trữ dự án' })).toHaveProperty('disabled', true);
    await act(async () => { fireEvent.click(await screen.findByRole('button', { name: 'Huỷ' })); });
    expect(client.api).not.toHaveBeenCalled();
    expect(screen.queryByTestId('where')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Lưu trữ dự án' }));
    await act(async () => { fireEvent.click(await screen.findByRole('button', { name: 'Lưu trữ' })); });
    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/projects'));
    expect(client.api).toHaveBeenCalledWith('DELETE', '/projects/' + P);
  });

  it('lưu trữ thất bại: ở lại trang và hiện lỗi của API', async () => {
    loaded();
    client.api.mockRejectedValue(new ApiError(403, { detail: 'thiếu quyền project:delete' }));
    show(`/projects/${P}/settings`);
    fireEvent.click(await screen.findByRole('button', { name: 'Lưu trữ dự án' }));
    await act(async () => { fireEvent.click(await screen.findByRole('button', { name: 'Lưu trữ' })); });
    await waitFor(() => expect(section('Vùng nguy hiểm').textContent).toContain('thiếu quyền project:delete'));
    expect(screen.queryByTestId('where')).toBeNull();
    expect(screen.getByRole('button', { name: 'Lưu trữ dự án' })).toHaveProperty('disabled', false);
  });
});
