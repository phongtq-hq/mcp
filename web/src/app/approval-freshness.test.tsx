// Bất biến của màn hình duyệt (security.md §4.1): người duyệt quyết định dựa trên đúng nội
// dung đang hiện, nên màn hình (1) luôn tải trạng thái hiện tại trước khi hiện gì, kể cả
// khi vừa xem approval đó; (2) không tự tải lại khi cửa sổ được chọn hay mạng có lại;
// (3) không hiện approval cũ trong lúc tải approval khác. Các bất biến này dựa vào cấu
// hình truy vấn (query-client.ts) và khoá truy vấn của trang: test dùng đúng cấu hình thật.
import { focusManager, onlineManager, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({ api: vi.fn(), get: vi.fn<(path: string) => Promise<unknown>>() }));
vi.mock('@/api/client', () => client);

import ApprovalDetailPage from '@/features/approvals/approval-detail-page';
import { newQueryClient } from './query-client';

const A = '0193a1b2-0000-7000-8000-00000000000a';
const B = '0193a1b2-0000-7000-8000-00000000000b';

function approval(id: string, tool: string) {
  return {
    id, project_id: '0193a1b2-0000-7000-8000-000000000002', subject_type: 'tool_call', risk_level: 'destructive',
    payload: { tool_name: tool, arguments: { target: tool } }, status: 'pending', required_approvals: 1, decisions: [],
    expires_at: new Date(Date.now() + 600_000).toISOString(), created_at: new Date().toISOString(),
  };
}

// Lời gọi API do test điều khiển: mỗi lần get trả một promise chưa xong.
let pending: { path: string; resolve: (value: unknown) => void }[] = [];
let goTo: (path: string) => void = () => {};

function Nav() {
  const navigate = useNavigate();
  goTo = path => { void navigate(path); };
  return null;
}

function show(path: string) {
  return render(
    <QueryClientProvider client={newQueryClient()}>
      <MemoryRouter initialEntries={[path]}>
        <Nav />
        <Routes>
          <Route path="approvals/:approvalId" element={<ApprovalDetailPage />} />
          <Route path="elsewhere" element={<p>nơi khác</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const answer = async (index: number, value: unknown) => { await act(async () => { pending[index]!.resolve(value); }); };
const loading = () => screen.queryByRole('status') !== null;

beforeEach(() => {
  pending = [];
  client.get.mockReset().mockImplementation(path => new Promise(resolve => { pending.push({ path, resolve }); }));
});
afterEach(() => {
  cleanup();
  focusManager.setFocused(undefined);
  onlineManager.setOnline(true);
});

describe('màn hình duyệt luôn hiện trạng thái vừa tải', () => {
  it('mở lại approval vừa xem: tải lại và hiện spinner, không hiện bản cũ', async () => {
    show('/approvals/' + A);
    await answer(0, approval(A, 'docker.restart_container'));
    expect(await screen.findByText('docker.restart_container')).toBeTruthy();

    await act(async () => { goTo('/elsewhere'); });
    await act(async () => { goTo('/approvals/' + A); });

    expect(pending).toHaveLength(2);
    expect(pending[1]!.path).toBe('/approvals/' + A);
    expect(screen.queryByText('docker.restart_container')).toBeNull();
    expect(loading()).toBe(true);

    await answer(1, { ...approval(A, 'docker.restart_container'), status: 'approved' });
    expect(await screen.findByText('docker.restart_container')).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Duyệt/ })).toBeNull();
  });

  it('chuyển sang approval khác: không hiện approval cũ trong lúc tải', async () => {
    show('/approvals/' + A);
    await answer(0, approval(A, 'docker.restart_container'));
    expect(await screen.findByText('docker.restart_container')).toBeTruthy();

    await act(async () => { goTo('/approvals/' + B); });

    expect(pending[pending.length - 1]!.path).toBe('/approvals/' + B);
    expect(screen.queryByText('docker.restart_container')).toBeNull();
    expect(screen.queryByRole('button', { name: /Duyệt/ })).toBeNull();
    expect(loading()).toBe(true);

    await answer(pending.length - 1, approval(B, 'github.merge_pull_request'));
    expect(await screen.findByText('github.merge_pull_request')).toBeTruthy();
  });

  it('cửa sổ được chọn lại hoặc mạng có lại: không tải lại', async () => {
    show('/approvals/' + A);
    await answer(0, approval(A, 'docker.restart_container'));
    expect(await screen.findByText('docker.restart_container')).toBeTruthy();
    const calls = client.get.mock.calls.length;

    await act(async () => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
      window.dispatchEvent(new Event('focus'));
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await waitFor(() => expect(screen.getByText('docker.restart_container')).toBeTruthy());

    expect(client.get.mock.calls.length).toBe(calls);
  });

  it('định danh sai dạng trên thanh địa chỉ không được ghép vào đường dẫn API', async () => {
    show('/approvals/..%2F..%2Fapi-keys');
    await act(async () => {});
    expect(client.get).not.toHaveBeenCalled();
  });
});
