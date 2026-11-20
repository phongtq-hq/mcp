import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({
  api: vi.fn<(method: string, path: string, body?: unknown, options?: unknown) => Promise<unknown>>(),
  getWithETag: vi.fn<(path: string) => Promise<unknown>>(),
}));
vi.mock('@/api/client', () => client);

import { ApiError } from '@/lib/api-error';
import OrganizationPage from './organization-page';

const org = (over: Record<string, unknown> = {}) => ({
  id: '0193a1b2-0000-7000-8000-000000000001', slug: 'acme', name: 'Acme', status: 'active',
  settings: { model_routing: { tiers: {} } }, created_at: '2026-11-19T00:00:00Z', updated_at: '2026-11-19T00:00:00Z', ...over,
});

const show = () => render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><OrganizationPage /></QueryClientProvider>);

beforeEach(() => { client.api.mockReset(); client.getWithETag.mockReset(); });
afterEach(cleanup);

describe('Organization', () => {
  const section = (name: string) => screen.getByRole('region', { name });
  const saveIn = (name: string) => fireEvent.click(section(name).querySelector('button[type=submit]')!);
  function loaded() {
    let n = 0;
    client.getWithETag.mockImplementation(async () => ({ data: org(), etag: `"o${++n}"` }));
  }

  it('hiện thông tin; đổi tên bằng PATCH kèm If-Match; đọc lại và báo đã lưu', async () => {
    loaded();
    client.api.mockResolvedValue(org({ name: 'Acme Corp' }));
    show();
    expect(await screen.findByText('acme')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: 'Acme Corp' } });
    saveIn('Thông tin chung');
    await waitFor(() => expect(client.api).toHaveBeenCalledWith('PATCH', '/organization', { name: 'Acme Corp' }, { ifMatch: '"o1"' }));
    await waitFor(() => expect(client.getWithETag).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('Đã lưu.')).toBeTruthy();
    // Mục JSON lưu sau đó dùng ETag của bản vừa đọc lại.
    await waitFor(() => expect(section('Cài đặt nâng cao (JSON)').querySelector('fieldset')!.disabled).toBe(false));
    saveIn('Cài đặt nâng cao (JSON)');
    await waitFor(() => expect(client.api).toHaveBeenCalledTimes(2));
    expect(client.api.mock.calls[1]).toEqual(['PATCH', '/organization', { settings: { model_routing: { tiers: {} } } }, { ifMatch: '"o2"' }]);
  });

  it('thiếu quyền đọc (403) và không tìm thấy (404) hiện thông báo, không hiện form', async () => {
    client.getWithETag.mockRejectedValueOnce(new ApiError(403, { detail: 'thiếu quyền' }));
    const first = show();
    expect((await screen.findByRole('alert')).textContent).toContain('không có quyền');
    expect(screen.queryByLabelText('Tên')).toBeNull();
    first.unmount();
    client.getWithETag.mockRejectedValueOnce(new ApiError(404, null));
    show();
    expect((await screen.findByRole('alert')).textContent).toContain('Không tìm thấy');
  });

  it('settings bị máy chủ từ chối (422): hiện nguyên thông báo, không báo đã lưu, giữ nội dung đang sửa', async () => {
    loaded();
    client.api.mockRejectedValue(new ApiError(422, { detail: 'model_routing: tier premium: model không tồn tại', code: 'validation_failed' }));
    show();
    const area = await screen.findByLabelText('Settings (JSON)');
    const text = '{"model_routing":{"tiers":{"premium":{"models":["x"]}}}}';
    fireEvent.change(area, { target: { value: text } });
    saveIn('Cài đặt nâng cao (JSON)');
    await waitFor(() => expect(section('Cài đặt nâng cao (JSON)').textContent).toContain('tier premium: model không tồn tại'));
    expect(client.api).toHaveBeenCalledWith('PATCH', '/organization', { settings: { model_routing: { tiers: { premium: { models: ['x'] } } } } }, { ifMatch: '"o1"' });
    expect(screen.queryByText('Đã lưu.')).toBeNull();
    expect(area).toHaveProperty('value', text);
    expect(client.getWithETag).toHaveBeenCalledTimes(1);
  });

  it('đang sửa JSON thì mục tên bị khoá; không có ETag thì từ chối lưu', async () => {
    client.getWithETag.mockResolvedValue({ data: org(), etag: null });
    show();
    fireEvent.change(await screen.findByLabelText('Settings (JSON)'), { target: { value: '{"a":1}' } });
    expect(section('Thông tin chung').querySelector('fieldset')!.disabled).toBe(true);
    saveIn('Cài đặt nâng cao (JSON)');
    expect(await screen.findByText(/máy chủ không gửi phiên bản/)).toBeTruthy();
    expect(client.api).not.toHaveBeenCalled();
  });
});
