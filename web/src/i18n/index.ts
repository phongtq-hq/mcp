// Ngôn ngữ giao diện. Tiếng Việt là mặc định; thêm ngôn ngữ = thêm một từ điển cùng bộ
// khoá (kiểu Record<Key, string> buộc điều đó lúc biên dịch).
import { useSyncExternalStore } from 'react';
import { en } from './en';
import { vi, type Key } from './vi';

export type { Key };

const dictionaries = { vi, en } as const;
export type Lang = keyof typeof dictionaries;

export const languages: readonly (readonly [Lang, string])[] = [['vi', 'Tiếng Việt'], ['en', 'English']];

function isLang(code: string): code is Lang {
  return Object.hasOwn(dictionaries, code);
}

let current: Lang = 'vi';
try {
  const saved = localStorage.getItem('hq.lang');
  if (saved && isLang(saved)) current = saved;
} catch { /* trình duyệt chặn localStorage: dùng mặc định */ }
document.documentElement.lang = current;

const listeners = new Set<() => void>();

export function lang(): Lang { return current; }

export function setLang(code: string): void {
  if (!isLang(code) || code === current) return;
  current = code;
  try { localStorage.setItem('hq.lang', code); } catch { /* bỏ qua */ }
  document.documentElement.lang = code;
  for (const l of listeners) l();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

// useLang: thành phần gốc đọc ngôn ngữ hiện tại và dựng lại cả cây khi nó đổi.
export function useLang(): Lang {
  return useSyncExternalStore(subscribe, lang);
}

function lookup(key: string): string | undefined {
  const dict: Record<string, string> = dictionaries[current];
  if (Object.hasOwn(dict, key)) return dict[key];
  const fallback: Record<string, string> = vi;
  return Object.hasOwn(fallback, key) ? fallback[key] : undefined;
}

// t trả về chuỗi của khoá key, thay {tên} bằng vars.tên.
export function t(key: Key, vars?: Record<string, string | number>): string {
  let s = lookup(key) ?? key;
  if (vars) for (const [k, v] of Object.entries(vars)) s = s.replaceAll('{' + k + '}', String(v));
  return s;
}

// tOr: như t, nhưng trả về fallback khi không có bản dịch (dùng cho giá trị do API trả
// về, ví dụ một trạng thái mới mà giao diện chưa biết).
export function tOr(key: string, fallback: string): string {
  return lookup(key) ?? fallback;
}

export function locale(): string { return current === 'vi' ? 'vi-VN' : 'en-GB'; }
