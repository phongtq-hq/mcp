// Bộ tách sự kiện Server-Sent Events (WHATWG HTML §9.2) từ các mẩu văn bản đến dần. Thuần,
// không gọi mạng: phần đọc luồng nằm ở api/events.ts.
export interface SSEEvent { event: string; data: string; id: string | null }

// Trần tính bằng ký tự (đơn vị UTF-16), không phải byte. Một sự kiện của platform chỉ mang
// định danh và trạng thái; dài hơn mức này là dấu hiệu luồng hỏng (hoặc không phải của platform), không phải thứ cần gom vào bộ nhớ.
export const MAX_SSE_EVENT_CHARS = 64 * 1024;

export class SSEOverflowError extends Error {
  constructor() { super('sse: event exceeds size limit'); this.name = 'SSEOverflowError'; }
}

// newSSEParser trả về hàm nhận từng mẩu văn bản; onEvent được gọi cho mỗi sự kiện hoàn chỉnh.
export function newSSEParser(onEvent: (event: SSEEvent) => void): (chunk: string) => void {
  let buffer = '';
  let event = '';
  let data: string[] = [];
  let id: string | null = null;
  let size = 0;

  const dispatch = () => {
    // Theo chuẩn: sự kiện không có dòng data nào thì bị bỏ.
    if (data.length > 0) onEvent({ event: event || 'message', data: data.join('\n'), id });
    event = '';
    data = [];
    size = 0;
  };

  const line = (text: string) => {
    if (text === '') { dispatch(); return; }
    if (text.startsWith(':')) return; // chú thích / giữ kết nối
    const colon = text.indexOf(':');
    const field = colon < 0 ? text : text.slice(0, colon);
    let value = colon < 0 ? '' : text.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    size += value.length;
    if (size > MAX_SSE_EVENT_CHARS) throw new SSEOverflowError();
    if (field === 'event') event = value;
    else if (field === 'data') data.push(value);
    else if (field === 'id' && !value.includes('\0')) id = value;
  };

  return chunk => {
    buffer += chunk;
    for (;;) {
      const match = /\r\n|\n|\r/.exec(buffer);
      if (!match) break;
      // "\r" ở cuối mẩu có thể là nửa đầu của "\r\n": chờ mẩu sau.
      if (match[0] === '\r' && match.index === buffer.length - 1) break;
      line(buffer.slice(0, match.index));
      buffer = buffer.slice(match.index + match[0].length);
    }
    if (buffer.length > MAX_SSE_EVENT_CHARS) throw new SSEOverflowError();
  };
}
