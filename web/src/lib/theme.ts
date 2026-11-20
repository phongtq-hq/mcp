// Giao diện tối / sáng. Tối là mặc định (tokens.css); lựa chọn của người dùng là một lựa chọn
// hiển thị, giữ trong localStorage như ngôn ngữ. Không có dữ liệu của tenant ở đây.
import { useSyncExternalStore } from 'react';

export type Theme = 'dark' | 'light';

const KEY = 'hq.theme';
const listeners = new Set<() => void>();

function read(): Theme {
  try { return localStorage.getItem(KEY) === 'light' ? 'light' : 'dark'; } catch { return 'dark'; }
}

let current: Theme = read();

function apply(): void {
  if (current === 'light') document.documentElement.dataset.theme = 'light';
  else delete document.documentElement.dataset.theme;
}
apply();

export function theme(): Theme { return current; }

export function setTheme(next: Theme): void {
  if (next === current) return;
  current = next;
  try { localStorage.setItem(KEY, next); } catch { /* trình duyệt chặn storage: chỉ đổi cho lượt này */ }
  apply();
  for (const l of listeners) l();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function useTheme(): Theme {
  return useSyncExternalStore(subscribe, theme);
}
