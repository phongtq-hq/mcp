import { describe, expect, it } from 'vitest';
import { json, visible } from './visible';

// Ký tự ẩn được dựng bằng mã số, để file test này không chứa ký tự không nhìn thấy.
const ch = (code: number) => String.fromCharCode(code);

describe('visible', () => {
  it('làm lộ ký tự đổi hướng chữ, rộng bằng không và điều khiển', () => {
    const cases: [number, string][] = [
      [0x202e, '\\u202E'], // RIGHT-TO-LEFT OVERRIDE
      [0x2066, '\\u2066'], // LEFT-TO-RIGHT ISOLATE
      [0x200b, '\\u200B'], // ZERO WIDTH SPACE
      [0x200d, '\\u200D'], // ZERO WIDTH JOINER
      [0xfeff, '\\uFEFF'], // BOM
      [0x00ad, '\\u00AD'], // SOFT HYPHEN
      [0x0000, '\\u0000'],
      [0x001b, '\\u001B'], // ESC
      [0x007f, '\\u007F'],
      [0x2028, '\\u2028'], // LINE SEPARATOR
    ];
    for (const [code, shown] of cases) {
      expect(visible('rm ' + ch(code) + 'x')).toBe('rm ' + shown + 'x');
    }
  });

  it('giữ nguyên xuống dòng, tab và chữ có dấu', () => {
    const text = 'dòng một\n\tdòng hai: tiếng Việt, 日本語, emoji 🙂';
    expect(visible(text)).toBe(text);
  });

  it('nhận giá trị không phải chuỗi', () => {
    expect(visible(null)).toBe('');
    expect(visible(undefined)).toBe('');
    expect(visible(42)).toBe('42');
  });

  it('json: ký tự ẩn trong giá trị cũng bị làm lộ', () => {
    const out = json({ path: '/tmp/' + ch(0x202e) + 'gpj.exe' });
    expect(out).toContain('\\u202E');
    expect(out).not.toContain(ch(0x202e));
  });
});
