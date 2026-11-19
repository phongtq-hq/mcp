import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Link, Outlet, useLocation } from 'react-router';
import { get, type Page, type Schemas } from '@/api/client';
import { pendingApprovalsKey } from '@/api/keys';
import * as auth from '@/auth/oidc';
import { t, type Key } from '@/i18n';
import { shortId } from '@/lib/format';
import { LanguageSelect } from './login';

const NAV: readonly (readonly [string, Key])[] = [
  ['/', 'navOverview'],
  ['/projects', 'navProjects'],
  ['/approvals', 'navApprovals'],
  ['/cost', 'navCost'],
  ['/mcp', 'navMcp'],
  ['/audit', 'navAudit'],
];

// navSection: mục điều hướng đang chọn. Chi tiết lần chạy thuộc mục Dự án.
function navSection(path: string): string {
  if (path.startsWith('/projects') || path.startsWith('/executions')) return '/projects';
  for (const [href] of NAV) if (href !== '/' && path.startsWith(href)) return href;
  return '/';
}

export function Shell() {
  const location = useLocation();
  const who = auth.claims();
  const section = navSection(location.pathname);

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
      <aside className="side">
        <div className="brand"><span className="brand-mark" aria-hidden="true">M</span>{t('appName')}</div>
        <nav className="nav" aria-label={t('appName')}>
          {NAV.map(([href, key]) => (
            <Link key={href} to={href} aria-current={href === section ? 'page' : undefined}>
              {t(key)}
              {key === 'navApprovals' && count > 0 ? <span className="count">{count}</span> : null}
            </Link>
          ))}
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
      <main className="main"><Outlet /></main>
    </div>
  );
}
