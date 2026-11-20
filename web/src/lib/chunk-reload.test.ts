import { beforeEach, describe, expect, it } from 'vitest';
import { shouldReload } from './chunk-reload';

beforeEach(() => sessionStorage.clear());

describe('shouldReload', () => {
  it('lần lỗi đầu thì tải lại; lỗi lại ngay sau đó thì không (tránh vòng lặp); đủ lâu thì lại được', () => {
    expect(shouldReload(1_000_000)).toBe(true);
    expect(shouldReload(1_000_500)).toBe(false);
    expect(shouldReload(1_059_999)).toBe(false);
    expect(shouldReload(1_060_001)).toBe(true);
  });
});
