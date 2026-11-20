import { t } from '@/i18n';

// Khung chờ có hình dạng của nội dung sắp hiện, để trang không nhảy khi dữ liệu về và người
// dùng biết thứ gì đang tải. `list`: một bảng. `detail`: hàng thẻ số liệu rồi các khối nội dung.
export function Skeleton({ kind = 'list' }: { kind?: 'list' | 'detail' }) {
  return (
    <div className="skeleton" role="status" aria-label={t('loading')}>
      {kind === 'detail' ? (
        <>
          <div className="sk sk-title" />
          <div className="grid">{[0, 1, 2].map(i => <div key={i} className="sk sk-card" />)}</div>
          <div className="sk sk-card" />
        </>
      ) : null}
      <div className="sk-table">
        <div className="sk-row head">{[0, 1, 2, 3].map(i => <div key={i} className="sk sk-line" />)}</div>
        {[0, 1, 2, 3, 4].map(r => <div key={r} className="sk-row">{[0, 1, 2, 3].map(i => <div key={i} className="sk sk-line" />)}</div>)}
      </div>
    </div>
  );
}
