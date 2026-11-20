import { Link } from 'react-router';
import { t, type Key } from '@/i18n';

const TABS: readonly (readonly [string, string, Key])[] = [
  ['models', '/models', 'tabModels'],
  ['providers', '/models/providers', 'tabProviders'],
  ['routing', '/models/routing', 'tabRouting'],
];

// Thanh tab của mục Mô hình. current là tab đang mở.
export function ModelsTabs({ current }: { current: 'models' | 'providers' | 'routing' }) {
  return (
    <nav className="tabs" aria-label={t('navModels')}>
      {TABS.map(([key, href, label]) => <Link key={key} to={href} aria-current={key === current ? 'page' : undefined}>{t(label)}</Link>)}
    </nav>
  );
}
