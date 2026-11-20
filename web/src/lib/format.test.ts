import { describe, expect, it } from 'vitest';
import { fmtRate, fmtTime, shortId } from './format';

describe('fmtRate', () => {
  it('bỏ số 0 thừa, giữ ít nhất hai chữ số lẻ, không làm tròn', () => {
    expect(fmtRate('0.10000000')).toBe('0.10');
    expect(fmtRate('15.00000000')).toBe('15.00');
    expect(fmtRate('0.10000001')).toBe('0.10000001');
    expect(fmtRate('3')).toBe('3.00');
    expect(fmtRate('3.75')).toBe('3.75');
  });
  it('thiếu giá trị hiện gạch; chuỗi lạ giữ nguyên', () => {
    expect(fmtRate(null)).toBe('—');
    expect(fmtRate(undefined)).toBe('—');
    expect(fmtRate('1e-7')).toBe('1e-7');
  });
});

describe('shortId', () => {
  it('lấy phần cuối: hai UUIDv7 tạo gần nhau vẫn khác nhau', () => {
    const a = '01a1f830-c746-78ed-a1b4-fc277414c4ca', b = '01a1f830-5b72-7bea-b207-4290951bb21b';
    expect(shortId(a)).toBe('7414c4ca');
    expect(shortId(a)).not.toBe(shortId(b));
    expect(shortId(null)).toBe('—');
  });
});

describe('fmtTime', () => {
  it('năm đủ bốn chữ số', () => {
    expect(fmtTime('2026-11-20T09:01:48Z')).toContain('2026');
    expect(fmtTime(null)).toBe('—');
  });
});
