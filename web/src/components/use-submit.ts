import { useCallback, useEffect, useRef, useState } from 'react';
import { fieldErrors } from '@/lib/api-error';

export interface Submit {
  // run gửi một lần; lần bấm thứ hai trong lúc đang gửi bị bỏ qua.
  run: () => Promise<void>;
  busy: boolean;
  // Lỗi của lần gửi gần nhất (null khi chưa gửi hoặc đã thành công).
  error: unknown;
  // Lỗi theo trường mà API trả về (400 / 422), dạng { tên trường: thông báo }.
  fields: Record<string, string>;
  reset: () => void;
}

// useSubmit: trạng thái của một hành động gửi lên API (lưu form, chạy, xoá). Kết quả hiện
// trên màn hình luôn là kết quả thật của API: thành công mới gọi onDone, lỗi thì giữ nguyên
// nội dung người dùng đã nhập và hiện lỗi.
export function useSubmit<T>(action: () => Promise<T>, onDone?: (result: T) => void | Promise<void>): Submit {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [fields, setFields] = useState<Record<string, string>>({});
  const running = useRef(false);
  // Bản mới nhất của action / onDone, ghi sau mỗi lượt dựng đã được áp (không ghi trong lúc dựng).
  const latest = useRef({ action, onDone });
  useEffect(() => { latest.current = { action, onDone }; });

  const run = useCallback(async () => {
    if (running.current) return;
    running.current = true;
    setBusy(true);
    setError(null);
    setFields({});
    let done: { result: T } | null = null;
    try {
      done = { result: await latest.current.action() };
    } catch (err) {
      setError(err);
      setFields(fieldErrors(err));
    }
    if (done) {
      // API đã thành công. Lỗi của bước sau đó (tải lại, điều hướng) KHÔNG phải lỗi của lần
      // gửi: hiện nó như lỗi gửi sẽ mời người dùng gửi lại một việc đã xong.
      try { await latest.current.onDone?.(done.result); } catch { /* màn hình tự tải lại ở lần mở sau */ }
    }
    running.current = false;
    setBusy(false);
  }, []);

  const reset = useCallback(() => { setError(null); setFields({}); }, []);
  return { run, busy, error, fields, reset };
}
