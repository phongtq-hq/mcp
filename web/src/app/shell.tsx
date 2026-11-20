// Khung trang của bảng điều khiển: thanh bên theo nhóm (thu gọn được), thanh trên (breadcrumb,
// bảng lệnh, approval đang chờ, giao diện sáng / tối, tài khoản) và vùng nội dung.
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Suspense, useEffect, useState } from 'react';
import { Link, Outlet, useLocation } from 'react-router';
import { get, type Page, type Schemas } from '@/api/client';
import { pendingApprovalsKey } from '@/api/keys';
import * as auth from '@/auth/oidc';
import AssistantBubble from '@/features/assistant/assistant-bubble';
import { Icon } from '@/components/icon';
import { Spinner } from '@/components/ui';
import { t } from '@/i18n';
import { shortId } from '@/lib/format';
import { setTheme, useTheme } from '@/lib/theme';
import { useStored } from '@/lib/use-stored';
import { CommandPalette } from './command-palette';
import { LanguageSelect } from './login';
import { activeItem, crumbs, NAV_GROUPS } from './navigation';

// Màn hình chiếm trọn vùng nội dung (không lề): bản đồ Agent là một canvas.
const FLUSH = ['/agents/map'];

export function Shell() {
  const location = useLocation();
  const who = auth.claims();
  const current = activeItem(location.pathname);
  const theme = useTheme();
  const [collapsed, setCollapsed] = useStored('hq.side', 'open');
  const [palette, setPalette] = useState(false);
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
  // Không đọc được approval (thiếu quyền): chuông không nêu con số nào.
  const bell = pending.error ? t('navApprovals') : t('pendingApprovalsCount', { count });

  // Ctrl / Cmd + K mở bảng lệnh ở mọi màn hình.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setPalette(open => !open);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const name = who.preferred_username || who.email || shortId(who.sub);
  const isCollapsed = collapsed === 'min';
  const flush = FLUSH.includes(location.pathname);

  return (
    <div className="shell" data-collapsed={isCollapsed || undefined}>
      <button type="button" className="skip" onClick={() => document.getElementById('main')?.focus()}>{t('skipToContent')}</button>
      <aside className={menuOpen ? 'side open' : 'side'}>
        <div className="brand">
          <span className="brand-mark" aria-hidden="true">M</span>
          <span className="brand-name">{t('appName')}</span>
        </div>
        <nav className="nav" id="side-nav" aria-label={t('appName')}>
          {NAV_GROUPS.map(group => (
            <div key={group.label} className="nav-section" role="group" aria-label={t(group.label)}>
              <div className="nav-group" aria-hidden="true">{t(group.label)}</div>
              {group.items.map(item => (
                <Link key={item.href} to={item.href} title={isCollapsed ? t(item.label) : undefined}
                  aria-current={item === current ? 'page' : undefined} onClick={closeMenu}>
                  <Icon name={item.icon} />
                  <span className="nav-label">{t(item.label)}</span>
                  {item.badge === 'approvals' && count > 0 ? <span className="count">{count}</span> : null}
                </Link>
              ))}
            </div>
          ))}
        </nav>
        <div className="side-foot">
          <button type="button" className="ghost side-collapse" aria-pressed={isCollapsed}
            title={t(isCollapsed ? 'sideExpand' : 'sideCollapse')} onClick={() => setCollapsed(isCollapsed ? 'open' : 'min')}>
            <Icon name="sidebar" /><span className="nav-label">{t(isCollapsed ? 'sideExpand' : 'sideCollapse')}</span>
          </button>
        </div>
      </aside>
      {/* Màn hình hẹp: bấm ra ngoài ngăn điều hướng thì thu nó lại. */}
      {menuOpen ? <div className="side-scrim" aria-hidden="true" onClick={closeMenu} /> : null}
      <div className="frame">
        <header className="topbar">
          <button type="button" className="menu-toggle" aria-expanded={menuOpen} aria-controls="side-nav"
            onClick={() => setMenu({ open: !menuOpen, at: location.pathname })}>{t('menu')}</button>
          <nav className="breadcrumbs" aria-label={t('breadcrumbs')}>
            <ol>
              {crumbs(location.pathname).map(c => (
                <li key={c.label}>{c.href ? <Link to={c.href}>{t(c.label)}</Link> : <span>{t(c.label)}</span>}</li>
              ))}
            </ol>
          </nav>
          <span className="spacer" />
          <button type="button" className="search-open" onClick={() => setPalette(true)}>
            <Icon name="search" /><span>{t('paletteOpen')}</span><kbd>Ctrl K</kbd>
          </button>
          <Link className="icon-btn" to="/approvals" aria-label={bell} title={bell}>
            <Icon name="bell" />{count > 0 ? <span className="dot" aria-hidden="true" /> : null}
          </Link>
          <button type="button" className="icon-btn" title={t(theme === 'dark' ? 'themeLight' : 'themeDark')} aria-label={t(theme === 'dark' ? 'themeLight' : 'themeDark')}
            onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>
            <Icon name={theme === 'dark' ? 'sun' : 'moon'} />
          </button>
          <details className="user-menu">
            <summary><Icon name="user" /><span className="who">{name}</span></summary>
            <div className="user-pop">
              <div>
                <div className="small muted">{t('signedInAs')}</div>
                <div className="who">{name}</div>
              </div>
              <LanguageSelect />
              <button onClick={() => auth.logout()}>{t('logout')}</button>
            </div>
          </details>
        </header>
        {/* Vùng chờ nằm TRONG khung trang: nạp một màn hình chưa tải không làm mất điều hướng. */}
        <main className={flush ? 'main flush' : 'main'} id="main" tabIndex={-1}>
          {/* key = mục điều hướng: sang mục khác thì nội dung hiện lên nhẹ; đổi tab trong cùng một
              mục thì không. Trang chi tiết phê duyệt (nơi ra quyết định) không có chuyển động nào. */}
          <div key={current.href} className={location.pathname.startsWith('/approvals/') ? 'no-motion' : 'page-enter'}>
            <Suspense fallback={<Spinner />}><Outlet /></Suspense>
          </div>
        </main>
      </div>
      <CommandPalette open={palette} onClose={() => setPalette(false)} />
      <AssistantBubble />
    </div>
  );
}
