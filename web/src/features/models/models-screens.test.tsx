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
import ModelPage from './model-page';
import ModelsPage from './models-page';
import ProviderPage from './provider-page';
import ProvidersPage from './providers-page';
import RoutingPage from './routing-page';

const PROV = '0193a1b2-0000-7000-8000-0000000000a1';
const PLATFORM = '0193a1b2-0000-7000-8000-0000000000a2';
const MODEL = '0193a1b2-0000-7000-8000-0000000000b1';
const SECRET = 'sk-test-khoa-bi-mat-123';

const provider = (over: Record<string, unknown> = {}) => ({ id: PROV, slug: 'acme-llm', adapter: 'anthropic', scope: 'organization', enabled: true, has_credential: false, settings: {}, ...over });
const model = (over: Record<string, unknown> = {}) => ({
  id: MODEL, provider_id: PROV, provider_model: 'model-x', display_name: 'Model X', tier: 'standard', enabled: true, has_current_pricing: false,
  capabilities: { tool_calling: true, context_window: 200000, vendor_flag: 'giữ nguyên' }, ...over,
});
const page = (data: unknown[]) => ({ data, next_cursor: null });

function Where() { return <div data-testid="where">{useLocation().pathname}</div>; }

function show(path: string) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/models" element={<ModelsPage />} />
          <Route path="/models/providers" element={<><ProvidersPage /><Where /></>} />
          <Route path="/models/providers/:providerId" element={<ProviderPage />} />
          <Route path="/models/routing" element={<RoutingPage />} />
          <Route path="/models/:modelId" element={<ModelPage />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const region = (name: string) => screen.getByRole('region', { name });
const submitIn = (name: string) => fireEvent.click(region(name).querySelector('button[type=submit]')!);
// Mọi văn bản đang hiện cùng giá trị của mọi ô nhập: nơi một bí mật có thể còn nằm lại.
const everything = () => (document.body.textContent ?? '') + Array.from(document.querySelectorAll('input, textarea'), el => (el as HTMLInputElement).value).join('\n');

beforeEach(() => { for (const fn of [client.api, client.get, client.getWithETag]) fn.mockReset(); });
afterEach(cleanup);

describe('Provider', () => {
  it('danh sách: provider của platform và của organization; thiếu quyền (403) hiện lỗi', async () => {
    client.get.mockResolvedValueOnce(page([provider({ has_credential: true }), provider({ id: PLATFORM, slug: 'platform-llm', scope: 'platform' })]));
    const first = show('/models/providers');
    expect(await screen.findByRole('link', { name: 'acme-llm' })).toBeTruthy();
    expect(screen.getByText('Toàn platform')).toBeTruthy();
    first.unmount();
    client.get.mockRejectedValueOnce(new ApiError(403, { detail: 'thiếu quyền model_provider:read' }));
    show('/models/providers');
    expect((await screen.findByRole('alert')).textContent).toContain('thiếu quyền model_provider:read');
  });

  it('tạo: gửi khoá một lần, rồi xoá khoá khỏi ô nhập và không để lại ở đâu trên trang', async () => {
    client.get.mockResolvedValue(page([]));
    client.api.mockResolvedValue(provider({ has_credential: true }));
    show('/models/providers');
    await screen.findByText('Chưa có provider nào.');
    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'acme-llm' } });
    fireEvent.change(screen.getByLabelText('Adapter'), { target: { value: 'anthropic' } });
    const key = screen.getByLabelText('Khoá API');
    expect(key.getAttribute('type')).toBe('password');
    expect(key.getAttribute('autocomplete')).toBe('new-password');
    fireEvent.change(key, { target: { value: SECRET } });
    submitIn('Thêm provider');
    expect(await screen.findByText('Đã thêm provider “acme-llm”.')).toBeTruthy();
    expect(client.api).toHaveBeenCalledWith('POST', '/model-providers', { slug: 'acme-llm', adapter: 'anthropic', api_key: SECRET });
    expect(screen.getByLabelText('Khoá API')).toHaveProperty('value', '');
    expect(everything()).not.toContain(SECRET);
  });

  it('máy chủ từ chối (422): lỗi hiện đúng ô và khoá bị xoá khỏi ô; lỗi không rõ kết quả (503): khoá được giữ để gửi lại', async () => {
    client.get.mockResolvedValue(page([]));
    show('/models/providers');
    await screen.findByText('Chưa có provider nào.');
    fireEvent.change(screen.getByLabelText('Slug'), { target: { value: 'p' } });
    fireEvent.change(screen.getByLabelText('Adapter'), { target: { value: 'anthropic' } });

    client.api.mockRejectedValueOnce(new ApiError(503, { detail: 'không kiểm được khoá với provider', code: 'provider_unavailable' }));
    fireEvent.change(screen.getByLabelText('Khoá API'), { target: { value: SECRET } });
    submitIn('Thêm provider');
    await waitFor(() => expect(region('Thêm provider').textContent).toContain('không kiểm được khoá'));
    expect(screen.getByLabelText('Khoá API')).toHaveProperty('value', SECRET);

    client.api.mockRejectedValueOnce(new ApiError(422, { detail: 'api_key: provider từ chối khoá này', errors: [{ field: 'api_key', message: 'api_key: provider từ chối khoá này' }] }));
    submitIn('Thêm provider');
    await waitFor(() => expect(screen.getByLabelText('Khoá API').getAttribute('aria-invalid')).toBe('true'));
    expect(screen.getByLabelText('Khoá API')).toHaveProperty('value', '');
    expect(everything()).not.toContain(SECRET);

    // Không nhập khoá: thân không có api_key.
    client.api.mockResolvedValueOnce(provider());
    submitIn('Thêm provider');
    await waitFor(() => expect(client.api).toHaveBeenCalledTimes(3));
    expect(client.api.mock.calls[2]![2]).toEqual({ slug: 'p', adapter: 'anthropic' });
  });

  it('không tìm thấy provider (404) và định danh sai dạng trên URL', async () => {
    client.getWithETag.mockRejectedValue(new ApiError(404, null));
    const first = show('/models/providers/' + PROV);
    expect((await screen.findByRole('alert')).textContent).toContain('Không tìm thấy');
    first.unmount();
    client.getWithETag.mockClear();
    client.get.mockResolvedValue(page([]));
    show('/models/providers/khong-phai-uuid');
    expect(await screen.findByTestId('where')).toHaveProperty('textContent', '/models/providers');
    expect(client.getWithETag).not.toHaveBeenCalled();
  });

  it('provider của platform: chỉ đọc, không có form sửa hay nút xoá', async () => {
    client.getWithETag.mockResolvedValue({ data: provider({ id: PLATFORM, slug: 'platform-llm', scope: 'platform' }), etag: '"p1"' });
    show('/models/providers/' + PLATFORM);
    expect(await screen.findByText(/do platform quản lý/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Lưu' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Xoá provider' })).toBeNull();
  });

  it('thay khoá: PATCH kèm If-Match; ô nhập bị xoá kể cả khi máy chủ từ chối; nút khoá khi ô trống', async () => {
    client.getWithETag.mockResolvedValue({ data: provider({ has_credential: true }), etag: '"p1"' });
    show('/models/providers/' + PROV);
    const input = await screen.findByLabelText('Khoá API mới');
    const button = region('Khoá API').querySelector<HTMLButtonElement>('button[type=submit]')!;
    expect(button.textContent).toBe('Thay khoá');
    expect(button.disabled).toBe(true);

    client.api.mockRejectedValueOnce(new ApiError(503, { detail: 'không kiểm được khoá với provider' }));
    fireEvent.change(input, { target: { value: SECRET } });
    fireEvent.click(button);
    await waitFor(() => expect(region('Khoá API').textContent).toContain('không kiểm được khoá với provider'));
    expect(client.api).toHaveBeenCalledWith('PATCH', '/model-providers/' + PROV, { api_key: SECRET }, { ifMatch: '"p1"' });
    expect(screen.getByLabelText('Khoá API mới')).toHaveProperty('value', '');
    expect(everything()).not.toContain(SECRET);
    expect(screen.queryByText('Đã lưu.')).toBeNull();
  });

  it('xoá: 409 provider_in_use hiện nguyên văn và ở lại trang; thành công thì về danh sách', async () => {
    client.getWithETag.mockResolvedValue({ data: provider(), etag: '"p1"' });
    client.get.mockResolvedValue(page([]));
    client.api.mockRejectedValueOnce(new ApiError(409, { detail: 'provider còn model; tắt provider (enabled=false) thay vì xoá', code: 'provider_in_use' }));
    show('/models/providers/' + PROV);
    fireEvent.click(await screen.findByRole('button', { name: 'Xoá provider' }));
    await act(async () => { fireEvent.click(await screen.findByRole('button', { name: 'Xoá' })); });
    await waitFor(() => expect(region('Vùng nguy hiểm').textContent).toContain('provider còn model'));
    expect(screen.queryByTestId('where')).toBeNull();

    client.api.mockResolvedValueOnce(null);
    fireEvent.click(screen.getByRole('button', { name: 'Xoá provider' }));
    await act(async () => { fireEvent.click(await screen.findByRole('button', { name: 'Xoá' })); });
    await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/models/providers'));
    expect(client.api).toHaveBeenLastCalledWith('DELETE', '/model-providers/' + PROV);
  });
});

describe('Model', () => {
  it('danh sách: tên provider, cảnh báo chưa có giá; tạo model chỉ dưới provider của organization', async () => {
    client.get.mockImplementation(async path => (path.startsWith('/model-providers')
      ? page([provider(), provider({ id: PLATFORM, slug: 'platform-llm', scope: 'platform' })])
      : page([model()])));
    client.api.mockResolvedValue(model({ display_name: 'Model Y' }));
    show('/models');
    expect(await screen.findByRole('link', { name: 'Model X' })).toBeTruthy();
    // Tên provider ở cột của bảng (thay cho id) và trong danh sách chọn của form tạo.
    await waitFor(() => expect(screen.getByRole('table').textContent).toContain('acme-llm'));
    expect(screen.getByText('Chưa có giá')).toBeTruthy();
    const select = screen.getByLabelText('Provider') as HTMLSelectElement;
    await waitFor(() => expect(Array.from(select.options).map(o => o.textContent)).toEqual(['— chọn —', 'acme-llm']));

    fireEvent.change(select, { target: { value: PROV } });
    fireEvent.change(screen.getByLabelText('Định danh phía provider'), { target: { value: ' model-y ' } });
    fireEvent.change(screen.getByLabelText('Tên hiển thị'), { target: { value: 'Model Y' } });
    fireEvent.change(screen.getByLabelText('Tier'), { target: { value: 'premium' } });
    fireEvent.click(screen.getByLabelText('Hỗ trợ gọi tool'));
    fireEvent.change(screen.getByLabelText('Cửa sổ ngữ cảnh'), { target: { value: '128000' } });
    submitIn('Thêm model');
    expect(await screen.findByText(/Đã thêm model “Model Y”/)).toBeTruthy();
    expect(client.api).toHaveBeenCalledWith('POST', '/models', {
      provider_id: PROV, provider_model: 'model-y', display_name: 'Model Y', tier: 'premium',
      capabilities: { tool_calling: true, structured_output: false, context_window: 128000 },
    });
  });

  it('sửa: PATCH kèm If-Match, giữ khoá capabilities mà form không biết, bỏ khoá số bị xoá trống', async () => {
    client.getWithETag.mockResolvedValue({ data: model(), etag: '"m1"' });
    client.get.mockResolvedValue(page([]));
    client.api.mockResolvedValue(model());
    show('/models/' + MODEL);
    fireEvent.change(await screen.findByLabelText('Tên hiển thị'), { target: { value: 'Model X2' } });
    // Model đã có tier: không có lựa chọn "Không có" (API không gỡ được tier).
    expect(Array.from((screen.getByLabelText('Tier') as HTMLSelectElement).options).map(o => o.textContent)).toEqual(['Tier economy', 'Tier standard', 'Tier premium']);
    fireEvent.change(screen.getByLabelText('Cửa sổ ngữ cảnh'), { target: { value: '' } });
    fireEvent.click(screen.getByLabelText('Bật model'));
    submitIn('Sửa');
    await waitFor(() => expect(client.api).toHaveBeenCalled());
    expect(client.api).toHaveBeenCalledWith('PATCH', '/models/' + MODEL, {
      display_name: 'Model X2', enabled: false, tier: 'standard',
      capabilities: { tool_calling: true, structured_output: false, vendor_flag: 'giữ nguyên' },
    }, { ifMatch: '"m1"' });
  });

  it('model chưa có tier: chọn được "Không có" và thân không gửi tier; 412 báo xung đột, không báo đã lưu', async () => {
    client.getWithETag.mockResolvedValue({ data: model({ tier: null }), etag: '"m1"' });
    client.get.mockResolvedValue(page([]));
    client.api.mockRejectedValue(new ApiError(412, { detail: 'tài nguyên đã thay đổi' }));
    show('/models/' + MODEL);
    const tier = await screen.findByLabelText('Tier') as HTMLSelectElement;
    expect(tier.options[0]!.textContent).toBe('Không có');
    submitIn('Sửa');
    await waitFor(() => expect(region('Sửa').textContent).toContain('đã bị thay đổi ở nơi khác'));
    expect(client.api.mock.calls[0]![2]).not.toHaveProperty('tier');
    expect(screen.queryByText('Đã lưu.')).toBeNull();
  });

  it('model của platform: máy chủ từ chối (403 platform_managed) và thông báo hiện nguyên văn', async () => {
    client.getWithETag.mockResolvedValue({ data: model(), etag: '"m1"' });
    client.get.mockResolvedValue(page([]));
    client.api.mockRejectedValue(new ApiError(403, { detail: 'tài nguyên do platform quản lý, không sửa được từ organization', code: 'platform_managed' }));
    show('/models/' + MODEL);
    await screen.findByLabelText('Tên hiển thị');
    submitIn('Sửa');
    await waitFor(() => expect(region('Sửa').textContent).toContain('do platform quản lý'));
  });

  it('bảng giá: lịch sử đủ các đơn giá và nguồn; thêm giá "áp dụng ngay" gửi chuỗi thập phân nguyên văn, không gửi thời điểm', async () => {
    client.getWithETag.mockResolvedValue({ data: model(), etag: '"m1"' });
    client.get.mockResolvedValue(page([{ id: 'pr1', model_id: MODEL, scope: 'list', version: 1, currency: 'USD', input_per_mtok: '3.00000000', output_per_mtok: '15.00000000', cache_write_per_mtok: '3.75000000', reasoning_per_mtok: '15.50000000', source: 'hợp đồng 2026', effective_from: '2026-01-01T00:00:00Z', effective_to: null }]));
    client.api.mockResolvedValue({});
    show('/models/' + MODEL);
    expect(await screen.findByText('3.00')).toBeTruthy();
    expect(screen.getByText('đang hiệu lực')).toBeTruthy();
    for (const text of ['3.75', '15.50', 'hợp đồng 2026']) expect(screen.getByText(text)).toBeTruthy();
    expect(screen.getByText(/chưa có giá hiệu lực/)).toBeTruthy();

    fireEvent.change(screen.getByLabelText('Đầu vào / 1 triệu token'), { target: { value: '0.10000001' } });
    fireEvent.change(screen.getByLabelText('Đầu ra / 1 triệu token'), { target: { value: '12.5' } });
    fireEvent.change(screen.getByLabelText('Nguồn'), { target: { value: 'bảng giá provider' } });
    submitIn('Bảng giá');
    await waitFor(() => expect(client.api).toHaveBeenCalled());
    // "Áp dụng ngay": không gửi giờ của trình duyệt, nên lần gửi lại có đúng thân này.
    expect(client.api).toHaveBeenCalledWith('POST', `/models/${MODEL}/pricing`, { currency: 'USD', input_per_mtok: '0.10000001', output_per_mtok: '12.5', source: 'bảng giá provider' });
  });

  it('thêm giá với thời điểm tự chọn: thiếu thời điểm thì không gọi API; máy chủ từ chối thì hiện lỗi ở ô', async () => {
    client.getWithETag.mockResolvedValue({ data: model({ has_current_pricing: true }), etag: '"m1"' });
    client.get.mockResolvedValue(page([]));
    show('/models/' + MODEL);
    fireEvent.change(await screen.findByLabelText('Đầu vào / 1 triệu token'), { target: { value: '1' } });
    fireEvent.change(screen.getByLabelText('Đầu ra / 1 triệu token'), { target: { value: '2' } });
    fireEvent.click(screen.getByLabelText('Áp dụng ngay'));
    const from = screen.getByLabelText('Hiệu lực từ');
    fireEvent.submit(region('Bảng giá').querySelector('form')!);
    await waitFor(() => expect(region('Bảng giá').textContent).toContain('Chọn thời điểm giá bắt đầu hiệu lực.'));
    expect(client.api).not.toHaveBeenCalled();

    client.api.mockRejectedValue(new ApiError(422, { detail: 'effective_from không được ở quá khứ', errors: [{ field: 'effective_from', message: 'effective_from không được ở quá khứ' }] }));
    fireEvent.change(from, { target: { value: '2020-01-01T00:00' } });
    submitIn('Bảng giá');
    await waitFor(() => expect(screen.getByLabelText('Hiệu lực từ').getAttribute('aria-invalid')).toBe('true'));
    expect((client.api.mock.calls[0]![2] as Record<string, string>).effective_from).toBe(new Date('2020-01-01T00:00').toISOString());
  });

  it('định danh sai dạng trên URL: quay về danh sách, không đọc model', async () => {
    client.get.mockResolvedValue(page([]));
    show('/models/khong-phai-uuid');
    expect(await screen.findByRole('heading', { name: 'Thêm model' })).toBeTruthy();
    expect(client.getWithETag).not.toHaveBeenCalled();
  });
});

describe('Định tuyến của organization', () => {
  const org = (settings: unknown) => ({ id: 'o1', slug: 'acme', name: 'Acme', status: 'active', settings });

  it('lưu chuỗi của tier bằng PATCH /organization kèm If-Match, giữ các khoá settings khác; báo đã lưu', async () => {
    let n = 0;
    client.getWithETag.mockImplementation(async () => ({ data: org({ khac: 1 }), etag: `"o${++n}"` }));
    client.get.mockResolvedValue(page([model({ display_name: 'Model X' })]));
    client.api.mockResolvedValue(org({}));
    show('/models/routing');
    const primary = (await screen.findAllByLabelText('Model chính'))[1]!; // tier standard
    fireEvent.change(primary, { target: { value: MODEL } });
    submitIn('Định tuyến theo tier');
    await waitFor(() => expect(client.api).toHaveBeenCalledWith('PATCH', '/organization', { settings: { khac: 1, model_routing: { tiers: { standard: { models: [MODEL] } } } } }, { ifMatch: '"o1"' }));
    expect(await screen.findByText('Đã lưu.')).toBeTruthy();
    expect(client.get).toHaveBeenCalledWith('/models?enabled=true&limit=200');
  });

  it('không có ETag: từ chối lưu; danh sách model còn trang sau: có ghi chú', async () => {
    client.getWithETag.mockResolvedValue({ data: org({}), etag: null });
    client.get.mockResolvedValue({ data: [model()], next_cursor: 'c1' });
    show('/models/routing');
    expect(await screen.findByText(/chỉ gồm 200 mục đầu/)).toBeTruthy();
    fireEvent.change(screen.getAllByLabelText('Model chính')[0]!, { target: { value: MODEL } });
    submitIn('Định tuyến theo tier');
    expect(await screen.findByText(/máy chủ không gửi phiên bản/)).toBeTruthy();
    expect(client.api).not.toHaveBeenCalled();
  });

  it('thiếu quyền đọc organization (403): hiện lỗi, không hiện form', async () => {
    client.getWithETag.mockRejectedValue(new ApiError(403, { detail: 'thiếu quyền organization:read' }));
    client.get.mockResolvedValue(page([]));
    show('/models/routing');
    expect((await screen.findByRole('alert')).textContent).toContain('thiếu quyền organization:read');
    expect(screen.queryByRole('button', { name: 'Lưu' })).toBeNull();
  });
});
