import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { streamExecutionEvents } from '@/api/events';

// Trần của việc nối lại: luồng của máy chủ dài tối đa 5 phút mỗi lần và mời nối lại; kết nối
// đứt thì thử lại thưa dần. Hết trần thì thôi: màn hình vẫn tự làm mới theo nhịp thường.
const MAX_RECONNECTS = 12;
const MAX_RETRIES = 4;
const RETRY_BASE_MS = 2000;
// Nhiều sự kiện tới sát nhau (một bước đóng, bước kế mở) chỉ gây một lượt tải lại.
const COALESCE_MS = 250;

// useExecutionStream: trong lúc lần chạy còn chạy, nghe luồng sự kiện của nó và tải lại dữ
// liệu ngay khi có sự kiện, để sơ đồ và danh sách bước đổi theo đúng lúc việc xảy ra chứ
// không chờ nhịp tự làm mới. Sự kiện chỉ mang định danh + trạng thái; nội dung vẫn đọc qua
// các endpoint thường. Trả về true khi luồng đang nối.
export function useExecutionStream(executionId: string, live: boolean): boolean {
  const client = useQueryClient();
  const [connected, setConnected] = useState(false);
  useEffect(() => {
    if (!live || !executionId) return;
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const refresh = () => {
      if (timer !== undefined) return;
      timer = setTimeout(() => { timer = undefined; void client.invalidateQueries({ queryKey: ['execution', executionId] }); }, COALESCE_MS);
    };
    const wait = (ms: number) => new Promise<void>(resolve => {
      const id = setTimeout(resolve, ms);
      abort.signal.addEventListener('abort', () => { clearTimeout(id); resolve(); }, { once: true });
    });
    void (async () => {
      let reconnects = 0, retries = 0;
      while (!abort.signal.aborted && reconnects <= MAX_RECONNECTS && retries <= MAX_RETRIES) {
        let end: Awaited<ReturnType<typeof streamExecutionEvents>>;
        try {
          end = await streamExecutionEvents(executionId, () => { setConnected(true); retries = 0; refresh(); }, abort.signal);
        } catch {
          break; // máy chủ từ chối luồng (hết suất, mất quyền): quay về tự làm mới thường
        }
        if (abort.signal.aborted) break;
        if (end === 'end') { refresh(); break; }
        if (end === 'error') break;
        if (end === 'reconnect') { reconnects++; continue; }
        retries++;
        await wait(RETRY_BASE_MS * 2 ** (retries - 1));
      }
      if (!abort.signal.aborted) setConnected(false);
    })();
    return () => { abort.abort(); if (timer !== undefined) clearTimeout(timer); setConnected(false); };
  }, [client, executionId, live]);
  return connected;
}
