import { afterEach, describe, expect, it } from 'vitest';
import { en } from './en';
import { lang, setLang, t, tOr } from './index';
import { vi } from './vi';

afterEach(() => setLang('vi'));

describe('từ điển', () => {
  // Kiểu Record<Key, string> của en.ts đã buộc điều này lúc biên dịch; test giữ nó cả
  // khi ai đó nới kiểu.
  it('tiếng Việt và tiếng Anh có cùng bộ khoá', () => {
    expect(Object.keys(en).sort()).toEqual(Object.keys(vi).sort());
    expect(Object.keys(vi).length).toBeGreaterThan(150);
  });

  it('không có chuỗi rỗng', () => {
    for (const [name, dict] of [['vi', vi], ['en', en]] as const) {
      for (const [key, value] of Object.entries(dict)) {
        expect(value, `${name}.${key}`).not.toBe('');
      }
    }
  });

  it('tham số {tên} giống nhau ở hai ngôn ngữ', () => {
    const vars = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map(m => m[1]).sort();
    const english: Record<string, string> = en;
    for (const [key, value] of Object.entries(vi)) {
      expect(vars(english[key] ?? ''), key).toEqual(vars(value));
    }
  });
});

describe('t', () => {
  it('mặc định là tiếng Việt và đổi được ngôn ngữ', () => {
    expect(lang()).toBe('vi');
    expect(t('logout')).toBe(vi.logout);
    setLang('en');
    expect(t('logout')).toBe(en.logout);
    expect(document.documentElement.lang).toBe('en');
  });

  it('bỏ qua mã ngôn ngữ không có', () => {
    setLang('xx');
    setLang('__proto__');
    expect(lang()).toBe('vi');
  });

  it('thay tham số', () => {
    expect(t('decisionsCount', { n: 1, need: 2 })).toBe(vi.decisionsCount.replace('{n}', '1').replace('{need}', '2'));
  });

  it('tOr rơi về giá trị gốc khi không có bản dịch, kể cả tên thuộc tính có sẵn của Object', () => {
    expect(tOr('st_trang_thai_moi', 'trang_thai_moi')).toBe('trang_thai_moi');
    expect(tOr('constructor', 'x')).toBe('x');
    expect(tOr('toString', 'y')).toBe('y');
  });
});
