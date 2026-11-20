import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/api/client', () => ({ get: vi.fn(async () => ({ data: [], next_cursor: null, settings: {} })) }));
vi.mock('@/auth/oidc', () => ({ claims: () => ({ preferred_username: 'dev' }), logout: vi.fn() }));

import { Shell } from './shell';

const show = () => render(
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter initialEntries={['/']}>
      <Routes>
        <Route element={<Shell />}>
          <Route index element={<h1>Trang đầu</h1>} />
          <Route path="cost" element={<h1>Trang chi phí</h1>} />
        </Route>
      </Routes>
    </MemoryRouter>
  </QueryClientProvider>,
);

afterEach(cleanup);

describe('Khung trang', () => {
  it('nút đầu tiên của trang đưa focus thẳng tới nội dung', () => {
    show();
    const skip = screen.getByRole('button', { name: 'Bỏ qua điều hướng' });
    expect(document.querySelector('button')).toBe(skip);
    fireEvent.click(skip);
    expect(document.activeElement).toBe(screen.getByRole('main'));
  });

  it('menu của màn hình hẹp: mở bằng nút, chọn một mục thì thu lại', () => {
    show();
    const menu = screen.getByRole('button', { name: 'Menu' });
    expect(menu.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(menu);
    expect(menu.getAttribute('aria-expanded')).toBe('true');
    fireEvent.click(screen.getByRole('link', { name: 'Chi phí' }));
    expect(screen.getByText('Trang chi phí')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Menu' }).getAttribute('aria-expanded')).toBe('false');
  });
});

describe('Khung trang: bảng lệnh và điều hướng theo nhóm', () => {
  it('Ctrl + K mở bảng lệnh; gõ để lọc, Enter để tới màn hình', async () => {
    show();
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const input = await screen.findByRole('combobox', { name: 'Bảng lệnh' });
    fireEvent.change(input, { target: { value: 'chi phi' } });
    expect(within(screen.getByRole('listbox', { name: 'Bảng lệnh' })).getAllByRole('option')).toHaveLength(1);
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(screen.getByText('Trang chi phí')).toBeTruthy();
    expect(screen.queryByRole('combobox', { name: 'Bảng lệnh' })).toBeNull();
  });

  it('thanh bên chia nhóm và thu gọn được; breadcrumb theo màn hình', () => {
    show();
    expect(screen.getByRole('group', { name: 'Điều khiển' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Bản đồ Agent' }).getAttribute('href')).toBe('/agents/map');
    const collapse = screen.getByRole('button', { name: 'Thu gọn thanh bên' });
    fireEvent.click(collapse);
    expect(screen.getByRole('button', { name: 'Mở rộng thanh bên' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(screen.getByRole('button', { name: 'Mở rộng thanh bên' }));
    expect(screen.getByRole('navigation', { name: 'Vị trí hiện tại' }).textContent).toContain('Tổng quan');
  });
});
