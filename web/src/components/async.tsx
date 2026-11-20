import type { ReactNode } from 'react';
import { t } from '@/i18n';
import { ErrorBox, Spinner } from './ui';

// Phần của một truy vấn mà màn hình cần (khớp với kết quả useQuery).
interface QueryLike<T> { data: T | undefined; error: unknown; isPending: boolean; isFetching?: boolean; refetch?: () => unknown }

// Async: lượt tải đầu hiện spinner; lượt tự làm mới giữ nội dung cũ tới khi có nội dung
// mới, để trang không nháy và các dòng không nhảy dưới con trỏ. Một lượt tải lỗi thay
// nội dung bằng thông báo lỗi: không để màn hình tiếp tục hiện dữ liệu đã cũ như còn đúng.
// Lỗi luôn có lối ra: nút thử lại (người dùng tự bấm, không tự thử lại), và `back` cho
// trang chi tiết (liên kết về danh sách) vì lúc lỗi trang không có tiêu đề hay breadcrumb.
export function Async<T>({ query, back, children }: { query: QueryLike<T>; back?: ReactNode; children: (data: T) => ReactNode }) {
  if (query.error) {
    const refetch = query.refetch;
    return (
      <div className="stack">
        <ErrorBox error={query.error} />
        <div className="row">
          {refetch ? <button type="button" disabled={query.isFetching} onClick={() => { void refetch(); }}>{t('retry')}</button> : null}
          {back}
        </div>
      </div>
    );
  }
  if (query.isPending || query.data === undefined) return <Spinner />;
  return <>{children(query.data)}</>;
}
