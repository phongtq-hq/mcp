import type { ReactNode } from 'react';
import { ErrorBox, Spinner } from './ui';

// Phần của một truy vấn mà màn hình cần (khớp với kết quả useQuery).
interface QueryLike<T> { data: T | undefined; error: unknown; isPending: boolean }

// Async: lượt tải đầu hiện spinner; lượt tự làm mới giữ nội dung cũ tới khi có nội dung
// mới, để trang không nháy và các dòng không nhảy dưới con trỏ. Một lượt tải lỗi thay
// nội dung bằng thông báo lỗi: không để màn hình tiếp tục hiện dữ liệu đã cũ như còn đúng.
export function Async<T>({ query, children }: { query: QueryLike<T>; children: (data: T) => ReactNode }) {
  if (query.error) return <ErrorBox error={query.error} />;
  if (query.isPending || query.data === undefined) return <Spinner />;
  return <>{children(query.data)}</>;
}
