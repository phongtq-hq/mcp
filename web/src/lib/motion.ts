// Chuyển động phía JavaScript đọc cùng token với CSS (styles/tokens.css): một nguồn duy nhất
// cho thời lượng. Không có số thời lượng nào khác trong component.
export type Duration = 'fast' | 'base' | 'slow' | 'flow';

const FALLBACK: Record<Duration, number> = { fast: 120, base: 180, slow: 280, flow: 900 };

// reducedMotion: người dùng xin giảm chuyển động, hoặc môi trường không cho biết (không có
// matchMedia, như trong test): khi đó mọi thứ đổi trạng thái tức thì.
export function reducedMotion(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return true;
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

export function durationMs(name: Duration): number {
  if (typeof document === 'undefined') return FALLBACK[name];
  const raw = getComputedStyle(document.documentElement).getPropertyValue('--dur-' + name).trim();
  const m = /^(\d+(?:\.\d+)?)(ms|s)$/.exec(raw);
  if (!m) return FALLBACK[name];
  return Number(m[1]) * (m[2] === 's' ? 1000 : 1);
}
