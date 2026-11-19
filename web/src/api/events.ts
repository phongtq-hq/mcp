// Luồng sự kiện của một execution (GET /v1/executions/{id}/events, agent-runtime §4.1).
// EventSource không gửi được header Authorization, nên luồng được đọc bằng fetch: token
// không bao giờ nằm trên URL.
import * as auth from '@/auth/oidc';
import { t } from '@/i18n';
import { ApiError, type ProblemLike } from '@/lib/api-error';
import { newSSEParser } from '@/lib/sse';

// Sự kiện chỉ mang định danh và trạng thái; màn hình tải nội dung qua các endpoint thường.
export interface ExecutionEvent { event: string; data: unknown }

// Lý do luồng kết thúc:
//   end        execution đã ở trạng thái kết thúc: không nối lại.
//   reconnect  máy chủ đóng luồng (hết 5 phút, hoặc nó dừng) và mời nối lại.
//   closed     kết nối đứt: nối lại được, nhưng người gọi phải tự giãn cách các lần thử.
//   error      máy chủ báo lỗi trên luồng rồi đóng (mất quyền đọc, lỗi nội bộ): KHÔNG nối
//              lại ngay; màn hình quay về tải thường và hiện lỗi của lần tải đó.
export type StreamEnd = 'end' | 'reconnect' | 'closed' | 'error';

export async function streamExecutionEvents(
  executionId: string,
  onEvent: (event: ExecutionEvent) => void,
  signal: AbortSignal,
): Promise<StreamEnd> {
  const token = await auth.accessToken();
  if (!token) { auth.expireSession(); throw new ApiError(401, { detail: t('errSession') }); }
  let res: Response;
  try {
    res = await fetch(auth.apiBase() + '/executions/' + encodeURIComponent(executionId) + '/events', {
      headers: { Authorization: 'Bearer ' + token, Accept: 'text/event-stream' },
      signal,
    });
  } catch {
    if (signal.aborted) return 'closed';
    throw new ApiError(0, { detail: t('errNetwork') });
  }
  if (res.status === 401) { auth.expireSession(); throw new ApiError(401, { detail: t('errSession') }); }
  if (!res.ok || !res.body) {
    let problem: unknown = null;
    try { problem = await res.json(); } catch { /* thân lỗi không phải JSON */ }
    throw new ApiError(res.status, problem && typeof problem === 'object' ? problem as ProblemLike : null);
  }

  const state: { end: StreamEnd } = { end: 'closed' };
  const parse = newSSEParser(ev => {
    // Sau sự kiện kết thúc đầu tiên, mọi thứ còn lại trong cùng mẩu bị bỏ.
    if (state.end !== 'closed') return;
    if (ev.event === 'end' || ev.event === 'reconnect' || ev.event === 'error') { state.end = ev.event; return; }
    let data: unknown = null;
    try { data = JSON.parse(ev.data) as unknown; } catch { /* sự kiện không có thân JSON */ }
    onEvent({ event: ev.event, data });
  });
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      parse(decoder.decode(value, { stream: true }));
      if (state.end !== 'closed') break;
    }
  } catch (err) {
    if (!signal.aborted) throw err;
  } finally {
    reader.cancel().catch(() => { /* luồng đã đóng */ });
  }
  return state.end;
}
