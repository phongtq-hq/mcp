import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({
  api: vi.fn<(method: string, path: string, body?: unknown) => Promise<unknown>>(),
  get: vi.fn(),
}));
vi.mock('@/api/client', () => client);

import type { Schemas } from '@/api/client';
import { t } from '@/i18n';
import { ApprovalDetail } from './approval-detail-page';

type Approval = Schemas['Approval'];

function approval(over: Partial<Approval> = {}): Approval {
  return {
    id: '0193a1b2-0000-7000-8000-000000000001',
    project_id: '0193a1b2-0000-7000-8000-000000000002',
    subject_type: 'tool_call',
    risk_level: 'destructive',
    payload: { tool_name: 'docker.restart_container', arguments: { container: 'web' } },
    status: 'pending',
    required_approvals: 1,
    decisions: [],
    expires_at: new Date(Date.now() + 600_000).toISOString(),
    created_at: new Date().toISOString(),
    ...over,
  };
}

function show(a: Approval) {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter><ApprovalDetail approval={a} /></MemoryRouter>
    </QueryClientProvider>,
  );
}

const approveButton = () => screen.getByRole<HTMLButtonElement>('button', { name: t('approve') });
const rejectButton = () => screen.getByRole<HTMLButtonElement>('button', { name: t('reject') });

beforeEach(() => {
  client.api.mockReset().mockResolvedValue({});
  vi.spyOn(window, 'confirm').mockReturnValue(true);
});
afterEach(cleanup);

describe('màn hình duyệt', () => {
  it('hiện tên tool và tham số đầy đủ, không có khung cuộn', () => {
    const { container } = show(approval());
    expect(screen.getByText('docker.restart_container')).toBeTruthy();
    const args = container.querySelector('pre.full');
    expect(args?.textContent).toBe(JSON.stringify({ container: 'web' }, null, 2));
    expect(approveButton().disabled).toBe(false);
    expect(rejectButton().disabled).toBe(false);
  });

  it('thiếu tham số: cảnh báo và khoá nút Duyệt, vẫn từ chối được', () => {
    show(approval({ payload: { tool_name: 'docker.restart_container' } }));
    expect(screen.getByText(t('approvalIncomplete'))).toBeTruthy();
    expect(approveButton().disabled).toBe(true);
    expect(rejectButton().disabled).toBe(false);
    fireEvent.click(approveButton());
    expect(client.api).not.toHaveBeenCalled();
  });

  it('thiếu tên tool: khoá nút Duyệt', () => {
    show(approval({ payload: { arguments: { container: 'web' } } }));
    expect(approveButton().disabled).toBe(true);
  });

  it('cổng workflow không có thông điệp: khoá nút Duyệt', () => {
    show(approval({ subject_type: 'workflow_step', payload: {} }));
    expect(approveButton().disabled).toBe(true);
  });

  it('tham số đã bị che một phần: có cảnh báo', () => {
    show(approval({ payload: { tool_name: 'github.create_secret', arguments: { value: '[REDACTED]' }, redactions: 2 } }));
    expect(screen.getByText(t('redactedArgs', { n: 2 }))).toBeTruthy();
  });

  it('ký tự ẩn trong tham số hiện thành \\uXXXX', () => {
    const rlo = String.fromCharCode(0x202e);
    const { container } = show(approval({ payload: { tool_name: 'docker.exec', arguments: { cmd: 'echo ' + rlo + 'ko' } } }));
    const shown = container.querySelector('pre.full')?.textContent ?? '';
    expect(shown).toContain('\\u202E');
    expect(shown).not.toContain(rlo);
  });

  it('nội dung do model sinh vào trang dạng văn bản, không thành phần tử', () => {
    const { container } = show(approval({ subject_type: 'workflow_step', payload: { message: '<img src=x onerror=alert(1)><b>đậm</b>' } }));
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('b')).toBeNull();
    expect(container.querySelector('.answer')?.textContent).toBe('<img src=x onerror=alert(1)><b>đậm</b>');
  });

  it('approval đã xử lý không có nút quyết định', () => {
    show(approval({ status: 'approved' }));
    expect(screen.queryByRole('button', { name: t('approve') })).toBeNull();
    expect(screen.queryByRole('button', { name: t('reject') })).toBeNull();
  });

  it('duyệt gửi đúng một quyết định kèm ghi chú đã cắt khoảng trắng', async () => {
    show(approval());
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '  đã kiểm tra  ' } });
    fireEvent.click(approveButton());
    await waitFor(() => expect(client.api).toHaveBeenCalledTimes(1));
    expect(client.api).toHaveBeenCalledWith('POST', '/approvals/0193a1b2-0000-7000-8000-000000000001/decisions', { decision: 'approve', comment: 'đã kiểm tra' });
  });

  it('không xác nhận thì không gửi gì', () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false);
    show(approval());
    fireEvent.click(rejectButton());
    expect(client.api).not.toHaveBeenCalled();
  });

  it('gửi lỗi: hiện lỗi và mở lại nút', async () => {
    client.api.mockRejectedValue(new Error('approval đã hết hạn'));
    show(approval());
    fireEvent.click(rejectButton());
    await waitFor(() => expect(screen.getByText(/approval đã hết hạn/)).toBeTruthy());
    expect(rejectButton().disabled).toBe(false);
    expect(approveButton().disabled).toBe(false);
  });
});
