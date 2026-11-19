import { t } from '@/i18n';
import { ErrorBox } from './ui';

// Phần của useInfiniteQuery mà nút cần.
interface PagedQuery {
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  fetchNextPage: () => unknown;
  // Lỗi khi tải trang kế tiếp (các trang đã tải vẫn hiện).
  isFetchNextPageError?: boolean;
  error?: unknown;
}

// LoadMore: tải trang kế tiếp của một danh sách phân trang keyset. Không còn trang thì không hiện gì.
export function LoadMore({ query }: { query: PagedQuery }) {
  if (!query.hasNextPage && !query.isFetchNextPageError) return null;
  return (
    <div className="stack">
      {query.isFetchNextPageError ? <ErrorBox error={query.error} /> : null}
      <div className="row">
        <button type="button" disabled={query.isFetchingNextPage} onClick={() => { void query.fetchNextPage(); }}>
          {query.isFetchingNextPage ? t('loading') : t('more')}
        </button>
      </div>
    </div>
  );
}
