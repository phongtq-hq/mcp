// Phân trang keyset của Management API (`limit`, `cursor`, `next_cursor`) cho useInfiniteQuery.
// Danh sách không bao giờ được tải hết rồi cắt ở trình duyệt: mỗi lần bấm "Tải thêm" là
// một trang của máy chủ.
import { get, type Page } from './client';

export function withCursor(path: string, cursor: string | null): string {
  if (!cursor) return path;
  return path + (path.includes('?') ? '&' : '?') + 'cursor=' + encodeURIComponent(cursor);
}

// pages: phần queryFn / pageParam của một truy vấn nhiều trang; người gọi thêm queryKey.
export function pages<T>(path: string) {
  return {
    queryFn: ({ pageParam }: { pageParam: string | null }) => get<Page<T>>(withCursor(path, pageParam)),
    initialPageParam: null as string | null,
    getNextPageParam: (last: Page<T>) => last.next_cursor,
  };
}

// rows: gộp các trang đã tải thành một danh sách.
export function rows<T>(data: { pages: Page<T>[] } | undefined): T[] {
  return data ? data.pages.flatMap(p => p.data) : [];
}
