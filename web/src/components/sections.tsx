// Trang có nhiều form cùng sửa MỘT tài nguyên (và dùng chung ETag của nó). Lưu một mục làm
// cả trang đọc lại tài nguyên, nên các mục khác phải dựng lại từ bản mới; để việc đó không
// làm mất nội dung đang gõ dở, tại một thời điểm chỉ một mục được sửa: mục đang sửa dở khoá
// các mục còn lại cho tới khi nó được lưu hoặc bỏ.
import { useCallback, useMemo, useState, type ReactNode } from 'react';
import { t } from '@/i18n';
import { Notice } from './ui';

export interface Sections {
  // Tên mục đang có thay đổi chưa lưu (null nếu không có).
  dirty: string | null;
  // Tên mục vừa lưu thành công (để hiện "Đã lưu." sau khi trang đọc lại).
  saved: string | null;
  // nonce đổi mỗi lần bỏ thay đổi: đưa vào `key` của mục để dựng lại form từ bản đã lưu.
  nonce: number;
  touch: (name: string) => void;
  markSaved: (name: string) => void;
  discard: () => void;
}

export function useSections(): Sections {
  const [dirty, setDirty] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const touch = useCallback((name: string) => { setDirty(name); setSaved(null); }, []);
  const markSaved = useCallback((name: string) => { setDirty(null); setSaved(name); }, []);
  const discard = useCallback(() => { setDirty(null); setSaved(null); setNonce(n => n + 1); }, []);
  return useMemo(() => ({ dirty, saved, nonce, touch, markSaved, discard }), [dirty, saved, nonce, touch, markSaved, discard]);
}

interface Props {
  name: string;
  title: string;
  sections: Sections;
  narrow?: boolean;
  children: ReactNode;
}

export function Section({ name, title, sections, narrow, children }: Props) {
  const locked = sections.dirty !== null && sections.dirty !== name;
  return (
    <section className={'card form' + (narrow ? ' narrow' : '')} aria-label={title}>
      <h2>{title}</h2>
      {locked ? <Notice kind="warn">{t('sectionLocked')}</Notice> : null}
      {/* fieldset disabled khoá mọi ô và nút bên trong, kể cả với bàn phím. */}
      <fieldset className="plain" disabled={locked}>{children}</fieldset>
      {sections.dirty === name ? (
        <div className="row"><button type="button" className="ghost" onClick={sections.discard}>{t('discardChanges')}</button></div>
      ) : null}
      {sections.saved === name ? <Notice kind="ok">{t('saved')}</Notice> : null}
    </section>
  );
}
