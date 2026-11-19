// Định dạng để HIỂN THỊ. Không có phép tính nghiệp vụ nào ở đây.
import { locale } from '@/i18n';

export function fmtTime(iso: string | null | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return new Intl.DateTimeFormat(locale(), { dateStyle: 'short', timeStyle: 'medium' }).format(d);
}

// fmtRelative: "3 phút trước", "trong 12 phút".
export function fmtRelative(iso: string | null | undefined): string {
  if (!iso) return '—';
  const diff = (new Date(iso).getTime() - Date.now()) / 1000;
  if (Number.isNaN(diff)) return '—';
  const rtf = new Intl.RelativeTimeFormat(locale(), { numeric: 'auto' });
  const abs = Math.abs(diff);
  if (abs < 60) return rtf.format(Math.round(diff), 'second');
  if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour');
  return rtf.format(Math.round(diff / 86400), 'day');
}

export function fmtDuration(startISO: string | null | undefined, endISO?: string | null): string {
  if (!startISO) return '—';
  const ms = (endISO ? new Date(endISO).getTime() : Date.now()) - new Date(startISO).getTime();
  if (!(ms >= 0)) return '—';
  if (ms < 1000) return ms + ' ms';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + ' s';
  const m = Math.floor(s / 60);
  return m < 60 ? `${m} m ${s % 60} s` : `${Math.floor(m / 60)} h ${m % 60} m`;
}

export function fmtNumber(n: unknown): string {
  return new Intl.NumberFormat(locale()).format(Number(n) || 0);
}

export interface MoneyLike { amount?: string | null; currency?: string | null }

// fmtMoney nhận {amount: "0.01234500", currency: "USD"}. Tiền là chuỗi thập phân ở API;
// chỉ đổi sang số để HIỂN THỊ, không cộng trừ ở trình duyệt.
export function fmtMoney(m: MoneyLike | null | undefined): string {
  if (!m || m.amount === undefined || m.amount === null) return '—';
  const v = Number(m.amount);
  if (Number.isNaN(v)) return String(m.amount);
  const digits = v !== 0 && Math.abs(v) < 0.01 ? 4 : 2;
  try {
    return new Intl.NumberFormat(locale(), { style: 'currency', currency: m.currency || 'USD', minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v);
  } catch {
    return v.toFixed(digits) + ' ' + (m.currency || '');
  }
}

export function shortId(id: string | null | undefined): string { return id ? String(id).slice(0, 8) : '—'; }

// isUuid: định danh lấy từ thanh địa chỉ chỉ được ghép vào đường dẫn API khi đúng dạng.
export function isUuid(s: string | undefined): s is string {
  return typeof s === 'string' && /^[0-9a-f-]{36}$/.test(s);
}

// localInputToISO: giá trị của <input type="datetime-local"> (giờ địa phương, không múi giờ)
// thành thời điểm RFC 3339 theo UTC. Trả null khi rỗng hoặc không đọc được.
export function localInputToISO(value: string): string | null {
  if (!value) return null;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}
