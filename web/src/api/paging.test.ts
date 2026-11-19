import { beforeEach, describe, expect, it, vi } from 'vitest';

const client = vi.hoisted(() => ({ get: vi.fn<(path: string) => Promise<unknown>>() }));
vi.mock('./client', () => client);

import { pages, rows, withCursor } from './paging';

beforeEach(() => { client.get.mockReset(); });

describe('paging', () => {
  it('withCursor nối cursor đã mã hoá vào đường dẫn có hoặc không có tham số', () => {
    expect(withCursor('/budgets', null)).toBe('/budgets');
    expect(withCursor('/budgets', 'a+b/c=')).toBe('/budgets?cursor=a%2Bb%2Fc%3D');
    expect(withCursor('/budgets?limit=50', 'x y')).toBe('/budgets?limit=50&cursor=x%20y');
  });

  it('pages gọi trang đầu không cursor, trang sau bằng next_cursor của máy chủ', async () => {
    const paged = pages<{ id: string }>('/api-keys?limit=2');
    client.get.mockResolvedValueOnce({ data: [{ id: 'a' }], next_cursor: 'c1' });
    const first = await paged.queryFn({ pageParam: paged.initialPageParam });
    expect(client.get).toHaveBeenLastCalledWith('/api-keys?limit=2');
    expect(paged.getNextPageParam(first)).toBe('c1');
    client.get.mockResolvedValueOnce({ data: [{ id: 'b' }], next_cursor: null });
    const second = await paged.queryFn({ pageParam: 'c1' });
    expect(client.get).toHaveBeenLastCalledWith('/api-keys?limit=2&cursor=c1');
    // null = hết trang (useInfiniteQuery coi null / undefined là không còn trang).
    expect(paged.getNextPageParam(second)).toBeNull();
    expect(rows({ pages: [first, second] })).toEqual([{ id: 'a' }, { id: 'b' }]);
    expect(rows(undefined)).toEqual([]);
  });
});
