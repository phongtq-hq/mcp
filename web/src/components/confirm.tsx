// Hộp xác nhận cho hành động có hậu quả (xoá, thu hồi, chạy, publish). Dùng <dialog> của
// trình duyệt: có bẫy tiêu điểm và đóng bằng Esc sẵn, không cần thư viện.
import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { t } from '@/i18n';
import { visible } from '@/lib/visible';

export interface ConfirmRequest {
  title: string;
  // title / body là văn bản và thường mang tên do tenant đặt mà người dùng dựa vào để quyết
  // định ("Xoá budget X?"): chúng được hiện qua visible() để ký tự ẩn không làm tên trông khác.
  body?: string;
  confirmLabel?: string;
  danger?: boolean;
}

// useConfirm trả về [phần tử phải đặt vào cây, hàm hỏi]. `await ask({...})` cho true khi
// người dùng xác nhận, false khi huỷ, bấm Esc hoặc một câu hỏi khác thay thế nó.
export function useConfirm(): [ReactNode, (request: ConfirmRequest) => Promise<boolean>] {
  const [request, setRequest] = useState<ConfirmRequest | null>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  const resolver = useRef<((ok: boolean) => void) | null>(null);
  const titleId = useId();

  const settle = useCallback((ok: boolean) => {
    const resolve = resolver.current;
    resolver.current = null;
    setRequest(null);
    resolve?.(ok);
  }, []);

  const ask = useCallback((next: ConfirmRequest) => new Promise<boolean>(resolve => {
    resolver.current?.(false);
    resolver.current = resolve;
    setRequest(next);
  }), []);

  useEffect(() => {
    const el = dialog.current;
    if (!el || !request) return;
    // jsdom và trình duyệt rất cũ không có showModal: khi đó hiện như một khối thường.
    if (typeof el.showModal === 'function') { if (!el.open) el.showModal(); } else el.setAttribute('open', '');
    return () => { if (typeof el.close === 'function' && el.open) el.close(); };
  }, [request]);

  // Rời màn hình trong lúc đang hỏi: coi như huỷ.
  useEffect(() => () => { resolver.current?.(false); resolver.current = null; }, []);

  const element = request ? (
    <dialog
      ref={dialog}
      className="confirm"
      aria-labelledby={titleId}
      onCancel={e => { e.preventDefault(); settle(false); }}
      // Đóng bằng đường nào khác (không qua nút, không qua Esc) cũng là huỷ: promise không bị treo.
      onClose={() => { if (resolver.current) settle(false); }}
    >
      <h2 id={titleId}>{visible(request.title)}</h2>
      {request.body ? <p className="answer">{visible(request.body)}</p> : null}
      <div className="row">
        {/* Huỷ đứng trước trong DOM: showModal() đưa tiêu điểm vào nó, nên Enter vô ý không
            kích hoạt hành động. */}
        <button type="button" onClick={() => settle(false)}>{t('cancel')}</button>
        <button type="button" className={request.danger ? 'danger' : 'primary'} onClick={() => settle(true)}>
          {request.confirmLabel ?? t('confirm')}
        </button>
      </div>
    </dialog>
  ) : null;

  return [element, ask];
}
