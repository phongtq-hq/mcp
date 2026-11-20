import { describe, expect, it } from 'vitest';
import { ApiError, fieldErrors, isStale } from './api-error';

describe('ApiError', () => {
  it('đọc lỗi theo trường và bỏ mục sai dạng', () => {
    const err = new ApiError(422, {
      detail: 'dữ liệu không hợp lệ', code: 'validation_failed',
      errors: [{ field: 'slug', code: 'pattern', message: 'chỉ chữ thường' }, { field: 'slug', message: 'tối đa 63 ký tự' }, { field: 1, message: 'x' }, 'rác', null, { field: 'name' }],
    });
    expect(err.fields).toHaveLength(2);
    expect(fieldErrors(err)).toEqual({ slug: 'chỉ chữ thường tối đa 63 ký tự' });
  });

  it('errors không phải mảng, hoặc lỗi không phải ApiError: không có lỗi theo trường', () => {
    expect(new ApiError(422, { errors: { field: 'x' } }).fields).toEqual([]);
    expect(fieldErrors(new Error('x'))).toEqual({});
    expect(fieldErrors(null)).toEqual({});
  });

  it('tên trường trùng thuộc tính có sẵn của Object không làm hỏng bản đồ', () => {
    expect(fieldErrors(new ApiError(422, { errors: [{ field: 'constructor', message: 'sai' }] }))).toEqual({ constructor: 'sai' });
  });

  it('có trần số lỗi theo trường', () => {
    const many = Array.from({ length: 500 }, (_, i) => ({ field: 'f' + i, message: 'm' }));
    expect(new ApiError(422, { errors: many }).fields).toHaveLength(100);
  });

  it('isStale: chỉ 412; 409 là xung đột nghiệp vụ và hiện thông báo của API', () => {
    expect(isStale(new ApiError(412, null))).toBe(true);
    expect(isStale(new ApiError(409, { code: 'slug_taken' }))).toBe(false);
    expect(isStale(new ApiError(422, null))).toBe(false);
    expect(isStale(new Error('x'))).toBe(false);
  });
});
