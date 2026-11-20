// Mô hình điều hướng của bảng điều khiển: một nguồn cho thanh bên, breadcrumb và bảng lệnh.
// Chỉ liệt kê màn hình ĐÃ CÓ và chạy trên API thật. Mục dự kiến (Live Monitor, Traces,
// Policies, Secrets, Team…) không có ở đây cho tới khi API của chúng tồn tại: xem
// .docs/ui-refactor-plan.md. Thêm màn hình = thêm một dòng vào đúng nhóm.
import type { IconName } from '@/components/icon';
import type { Key } from '@/i18n';

export interface NavItem {
  href: string;
  label: Key;
  icon: IconName;
  // Tiền tố đường dẫn khác cũng thuộc mục này (chi tiết lần chạy thuộc Dự án).
  also?: readonly string[];
  // Mục có số đếm approval đang chờ.
  badge?: 'approvals';
}

export interface NavGroup { label: Key; items: readonly NavItem[] }

export const NAV_GROUPS: readonly NavGroup[] = [
  { label: 'navGroupOverview', items: [
    { href: '/', label: 'navOverview', icon: 'home' },
  ] },
  { label: 'navGroupControl', items: [
    { href: '/agents', label: 'navAgents', icon: 'bot' },
    { href: '/agents/map', label: 'navAgentMap', icon: 'map' },
    { href: '/projects', label: 'navProjects', icon: 'folder', also: ['/executions'] },
    { href: '/mcp', label: 'navMcp', icon: 'server' },
    { href: '/models', label: 'navModels', icon: 'cpu' },
  ] },
  { label: 'navGroupGovernance', items: [
    { href: '/approvals', label: 'navApprovals', icon: 'shield', badge: 'approvals' },
    { href: '/audit', label: 'navAudit', icon: 'list' },
  ] },
  { label: 'navGroupAnalytics', items: [
    { href: '/cost', label: 'navCost', icon: 'coins' },
  ] },
  { label: 'navAdmin', items: [
    { href: '/admin/organization', label: 'navOrganization', icon: 'building' },
  ] },
];

export const NAV_ITEMS: readonly NavItem[] = NAV_GROUPS.flatMap(g => g.items);

function under(path: string, prefix: string): boolean {
  return path === prefix || path.startsWith(prefix + '/');
}

function matches(item: NavItem, path: string): boolean {
  if (item.href === '/') return path === '/';
  return under(path, item.href) || (item.also ?? []).some(p => under(path, p));
}

// activeItem: mục điều hướng của đường dẫn đang mở. Khi nhiều mục khớp (/agents và
// /agents/map), mục có đường dẫn dài nhất thắng.
export function activeItem(path: string): NavItem {
  let best: NavItem | null = null;
  for (const item of NAV_ITEMS) {
    if (matches(item, path) && (!best || item.href.length > best.href.length)) best = item;
  }
  return best ?? NAV_ITEMS[0]!;
}

export interface Crumb { label: Key; href: string | null }

// crumbs: đường đi từ nhóm tới màn hình đang mở. Mục cuối là liên kết khi người dùng đang ở
// sâu hơn nó (trang chi tiết), để quay lại danh sách bằng một cú bấm.
export function crumbs(path: string): Crumb[] {
  const item = activeItem(path);
  const group = NAV_GROUPS.find(g => g.items.includes(item));
  const out: Crumb[] = group ? [{ label: group.label, href: null }] : [];
  out.push({ label: item.label, href: path === item.href ? null : item.href });
  return out;
}
