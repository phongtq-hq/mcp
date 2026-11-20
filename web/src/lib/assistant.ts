// Phần thuần của trợ lý trong bảng điều khiển: đọc cấu hình, dựng ngữ cảnh màn hình và
// dựng tin nhắn gửi cho Agent trợ lý. Không gọi mạng.
import { isUuid } from './format';

// Trợ lý là một Agent thường của organization, chỉ ra bằng khoá `console_assistant` trong
// settings của organization. Thiếu hoặc sai dạng thì không có trợ lý.
export interface AssistantTarget { projectId: string; agentId: string }

export function assistantTarget(settings: unknown): AssistantTarget | null {
  if (!settings || typeof settings !== 'object') return null;
  const raw = (settings as Record<string, unknown>).console_assistant;
  if (!raw || typeof raw !== 'object') return null;
  const { project_id: projectId, agent_id: agentId } = raw as Record<string, unknown>;
  if (typeof projectId !== 'string' || typeof agentId !== 'string' || !isUuid(projectId) || !isUuid(agentId)) return null;
  return { projectId, agentId };
}

// screenPath: đường dẫn của màn hình đang mở với mọi định danh thay bằng `:id`. Trợ lý chỉ
// cần biết người dùng đang ở LOẠI màn hình nào; định danh của tenant không được gửi đi.
export function screenPath(pathname: string): string {
  const parts = pathname.split('/').filter(Boolean).map(part => (/^[a-z][a-z-]*$/.test(part) ? part : ':id'));
  return '/' + parts.join('/');
}

export interface Turn { question: string; answer: string }

export const MAX_QUESTION = 2000;
// Trần của cả tin nhắn gửi đi (các lượt cũ + câu hỏi mới). Lượt cũ nhất bị bỏ trước.
export const MAX_MESSAGE = 8000;
const MAX_HISTORY = 4;

// Thẻ do giao diện dựng. Nội dung người dùng gõ và trả lời cũ của model không được đóng hay
// mở các thẻ này: dấu `<` của chúng bị vô hiệu hoá trước khi ghép.
function inert(text: string): string {
  return text.replaceAll('<', '‹');
}

// buildMessage: tin nhắn của một câu hỏi. Platform chưa có memory theo phiên, nên vài lượt
// gần nhất được gửi lại để trợ lý hiểu câu hỏi nối tiếp.
export function buildMessage(pathname: string, history: readonly Turn[], question: string): string {
  const head = `<screen_context>\npath: ${screenPath(pathname)}\n</screen_context>\n\n`;
  const tail = `<question>\n${inert(question.slice(0, MAX_QUESTION))}\n</question>`;
  let turns = history.slice(-MAX_HISTORY);
  for (;;) {
    const past = turns.length === 0 ? '' :
      '<conversation>\n' + turns.map(x => `user: ${inert(x.question)}\nassistant: ${inert(x.answer)}`).join('\n\n') + '\n</conversation>\n\n';
    const message = head + past + tail;
    if (message.length <= MAX_MESSAGE || turns.length === 0) return message;
    turns = turns.slice(1);
  }
}
