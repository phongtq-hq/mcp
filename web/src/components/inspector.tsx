// Inspector: ngăn bên phải hiện chi tiết của thứ đang chọn mà KHÔNG rời màn hình hiện tại
// (bản đồ, danh sách). Không phải hộp thoại: phần còn lại của trang vẫn thao tác được. Esc
// đóng nó khi tiêu điểm đang ở trong ngăn. Mọi màn hình dùng chung một cỡ và một cách đóng.
import { useEffect, useRef, type ReactNode } from 'react';
import { t } from '@/i18n';
import { Icon, type IconName } from './icon';

interface Props {
  title: ReactNode;
  subtitle?: ReactNode;
  icon?: IconName;
  onClose: () => void;
  children: ReactNode;
}

export function Inspector({ title, subtitle, icon, onClose, children }: Props) {
  const ref = useRef<HTMLElement>(null);
  // Mở ngăn thì tiêu điểm vào ngăn (một lần, lúc mở): Esc đóng được ngay và trình đọc màn hình
  // đọc nội dung mới. Không lặp lại ở các lượt dựng sau, để không giật tiêu điểm khỏi ô đang gõ.
  useEffect(() => { ref.current?.focus(); }, []);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.stopPropagation(); onClose(); } };
    el.addEventListener('keydown', onKey);
    return () => el.removeEventListener('keydown', onKey);
  }, [onClose]);
  return (
    <aside ref={ref} className="inspector" tabIndex={-1} aria-label={t('inspector')}>
      <header className="inspector-head">
        {icon ? <span className="inspector-icon"><Icon name={icon} /></span> : null}
        <div className="inspector-title">
          <h2>{title}</h2>
          {subtitle ? <div className="small muted">{subtitle}</div> : null}
        </div>
        <button type="button" className="ghost icon-btn" aria-label={t('close')} title={t('close')} onClick={onClose}><Icon name="close" /></button>
      </header>
      <div className="inspector-body">{children}</div>
    </aside>
  );
}

// Một mục của inspector: tiêu đề nhỏ + nội dung.
export function InspectorSection({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="inspector-section">
      <h3>{title}</h3>
      {children}
    </section>
  );
}
