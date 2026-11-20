import { t } from '@/i18n';
import { json } from '@/lib/visible';

export { messageOf, TERMINAL } from '@/lib/execution';

export function modeLabel(mode: string): string {
  if (mode === 'single_agent') return t('modeSingle');
  if (mode === 'planned') return t('modePlanned');
  if (mode === 'direct_tool') return t('modeDirect');
  return mode;
}

// problemText: lỗi của execution / task thành một dòng. Lỗi là dữ liệu từ API, dạng
// không được giả định.
export function problemText(err: unknown): string | null {
  if (!err) return null;
  if (typeof err === 'string') return err;
  if (typeof err !== 'object') return String(err);
  const e = err as Record<string, unknown>;
  const str = (v: unknown) => (typeof v === 'string' && v ? v : null);
  return [str(e.code), str(e.message) ?? str(e.detail)].filter(Boolean).join(': ') || json(err);
}
