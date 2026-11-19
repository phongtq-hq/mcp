import { describe, expect, it } from 'vitest';
import { parseJson, parseJsonObject, pretty } from './json';

describe('json', () => {
  it('parseJson báo cú pháp sai và chuỗi rỗng', () => {
    expect(parseJson('{"a":1}')).toEqual({ ok: true, value: { a: 1 } });
    expect(parseJson('[1]')).toEqual({ ok: true, value: [1] });
    expect(parseJson('{a:1}').ok).toBe(false);
    expect(parseJson('   ').ok).toBe(false);
  });

  it('parseJsonObject chỉ nhận object', () => {
    expect(parseJsonObject('{"steps":[]}')).toEqual({ ok: true, value: { steps: [] } });
    for (const text of ['[]', 'null', '1', '"x"', '']) expect(parseJsonObject(text).ok).toBe(false);
  });

  it('pretty thụt lề và không ném lỗi', () => {
    expect(pretty({ a: 1 })).toBe('{\n  "a": 1\n}');
    expect(pretty(undefined)).toBe('{}');
    const loop: Record<string, unknown> = {};
    loop.self = loop;
    expect(pretty(loop)).toBe('{}');
  });
});
