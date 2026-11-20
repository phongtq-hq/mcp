import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Suspense, useState } from 'react';
import { Link, Outlet, useLocation } from 'react-router';
import { get, type Page, type Schemas } from '@/api/client';
import { pendingApprovalsKey } from '@/api/keys';
import * as auth from '@/auth/oidc';
import AssistantBubble from '@/features/assistant/assistant-bubble';
import { Spinner } from '@/components/ui';
import { t, type Key } from '@/i18n';
import { shortId } from '@/lib/format';
import { LanguageSelect } from './login';

type NavItem = readonly [string, Key];

const NAV: readonly NavItem[] = [
  ['/', 'navOverview'],
  ['/projects', 'navProjects'],
  ['/approvals', 'navApprovals'],
  ['/cost', 'navCost'],
  ['/models', 'navModels'],
  ['/mcp', 'navMcp'],
];
// Nhóm Quản trị: cấu hình của cả organization.
const ADMIN: readonly NavItem[] = [
  ['/admin/organization', 'navOrganization'],
  ['/audit', 'navAudit'],
];
const ALL: readonly NavItem[] = [...NAV, ...ADMIN];

// navSection: mục điều hướng đang chọn. Chi tiết lần chạy thuộc mục Dự án.
function navSection(path: string): string {
  if (path.startsWith('/projects') || path.startsWith('/executions')) return '/projects';
  for (const [href] of ALL) if (href !== '/' && path.startsWith(href)) return href;
  return '/';
}

export function Shell() {
  const location = useLocation();
  const who = auth.claims();
  const section = navSection(location.pathname);
  // Màn hình hẹp: điều hướng thu gọn sau một nút; chọn một mục thì thu lại.
  // Menu chỉ mở ở đúng màn hình nó được mở: mọi cách rời màn hình đó đều thu nó lại.
  const [menu, setMenu] = useState({ open: false, at: '' });
  const menuOpen = menu.open && menu.at === location.pathname;
  const closeMenu = () => setMenu({ open: false, at: '' });

  // Số approval đang chờ ở thanh điều hướng: đọc lại mỗi lần chuyển trang. Không có quyền
  // đọc approval thì không hiện số.
  const pending = useQuery({
    queryKey: [...pendingApprovalsKey, location.key],
    queryFn: () => get<Page<Schemas['Approval']>>('/approvals?status=pending&limit=50'),
    placeholderData: keepPreviousData,
    gcTime: 0,
  });
  const count = pending.error ? 0 : pending.data?.data.length ?? 0;

  return (
    <div className="shell">
      <button type="button" className="skip" onClick={() => document.getElementById('main')?.focus()}>{t('skipToContent')}</button>
      <aside className={menuOpen ? 'side open' : 'side'}>
        <div className="brand">
          <span className="brand-mark" aria-hidden="true">M</span>{t('appName')}
          <span className="spacer" />
          <button type="button" className="menu-toggle" aria-expanded={menuOpen} aria-controls="side-nav"
            onClick={() => setMenu({ open: !menuOpen, at: location.pathname })}>{t('menu')}</button>
        </div>
        <nav className="nav" id="side-nav" aria-label={t('appName')}>
          {NAV.map(([href, key]) => (
            <Link key={href} to={href} aria-current={href === section ? 'page' : undefined} onClick={closeMenu}>
              {t(key)}
              {key === 'navApprovals' && count > 0 ? <span className="count">{count}</span> : null}
            </Link>
          ))}
          <div className="nav-section" role="group" aria-label={t('navAdmin')}>
            <div className="nav-group" aria-hidden="true">{t('navAdmin')}</div>
            {ADMIN.map(([href, key]) => (
              <Link key={href} to={href} aria-current={href === section ? 'page' : undefined} onClick={closeMenu}>{t(key)}</Link>
            ))}
          </div>
        </nav>
        <div className="side-foot">
          <div>
            <div className="small">{t('signedInAs')}</div>
            <div className="who">{who.preferred_username || who.email || shortId(who.sub)}</div>
          </div>
          <LanguageSelect />
          <button onClick={() => auth.logout()}>{t('logout')}</button>
        </div>
      </aside>
      {/* Vùng chờ nằm TRONG khung trang: nạp một màn hình chưa tải không làm mất điều hướng. */}
      <main className="main" id="main" tabIndex={-1}>
        {/* key = mục điều hướng: sang mục khác thì nội dung hiện lên nhẹ; đổi tab trong cùng một
            mục thì không. Trang chi tiết phê duyệt (nơi ra quyết định) không có chuyển động nào. */}
        <div key={section} className={location.pathname.startsWith('/approvals/') ? 'no-motion' : 'page-enter'}>
          <Suspense fallback={<Spinner />}><Outlet /></Suspense>
        </div>
      </main>
      <AssistantBubble />
    </div>
  );
}
