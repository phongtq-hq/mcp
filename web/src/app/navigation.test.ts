import { describe, expect, it } from 'vitest';
import { en } from '@/i18n/en';
import { vi } from '@/i18n/vi';
import { activeItem, crumbs, NAV_GROUPS, NAV_ITEMS } from './navigation';

describe('điều hướng', () => {
  it('mỗi mục có đường dẫn riêng và có nhãn ở cả hai từ điển', () => {
    expect(new Set(NAV_ITEMS.map(i => i.href)).size).toBe(NAV_ITEMS.length);
    for (const key of [...NAV_ITEMS.map(i => i.label), ...NAV_GROUPS.map(g => g.label)]) {
      expect(vi[key]).toBeTruthy();
      expect(en[key]).toBeTruthy();
    }
  });

  it('mục đang chọn: đường dẫn dài nhất thắng; chi tiết lần chạy thuộc Dự án', () => {
    expect(activeItem('/').href).toBe('/');
    expect(activeItem('/agents').href).toBe('/agents');
    expect(activeItem('/agents/map').href).toBe('/agents/map');
    expect(activeItem('/projects/abc/settings').href).toBe('/projects');
    expect(activeItem('/executions/abc').href).toBe('/projects');
    expect(activeItem('/models/providers').href).toBe('/models');
    // Tiền tố phải là cả một đoạn đường dẫn.
    expect(activeItem('/costly').href).toBe('/');
  });

  it('breadcrumb: nhóm rồi màn hình; màn hình là liên kết khi đang ở sâu hơn nó', () => {
    expect(crumbs('/cost')).toEqual([{ label: 'navGroupAnalytics', href: null }, { label: 'navCost', href: null }]);
    expect(crumbs('/approvals/abc')).toEqual([{ label: 'navGroupGovernance', href: null }, { label: 'navApprovals', href: '/approvals' }]);
  });
});
