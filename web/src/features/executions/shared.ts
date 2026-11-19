import { t } from '@/i18n';
import { json } from '@/lib/visible';

// Trạng thái kết thúc: execution ở các trạng thái này không còn đổi, màn hình thôi tự làm mới.
export const TERMINAL: ReadonlySet<string> = new Set(['completed', 'failed', 'cancelled', 'budget_exceeded', 'timed_out']);

export function modeLabel(mode: string): string {
  if (mode === 'single_agent') return t('modeSingle');
  if (mode === 'planned') return t('modePlanned');
  if (mode === 'direct_tool') return t('modeDirect');
  return mode;
}

// messageOf: trường `message` của input / output, nếu là chuỗi.
export function messageOf(value: unknown): string {
  if (!value || typeof value !== 'object') return '';
  const m = (value as Record<string, unknown>).message;
  return typeof m === 'string' ? m : '';
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
