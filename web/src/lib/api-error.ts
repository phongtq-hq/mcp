// ApiError: lỗi của một lời gọi Management API (RFC 9457). status = 0 khi không tới được máy chủ.
export interface FieldProblem { field: string; code?: string; message: string }
export interface ProblemLike { detail?: string; title?: string; code?: string; errors?: unknown; trace_id?: unknown }

export class ApiError extends Error {
  readonly status: number;
  readonly code: string | undefined;
  // Lỗi theo trường của một phản hồi 400 / 422 (`errors[]` trong thân lỗi); rỗng nếu không có.
  readonly fields: readonly FieldProblem[];
  // Mã trace của request lỗi, để người dùng gửi cho người vận hành.
  readonly traceId: string | undefined;

  constructor(status: number, problem: ProblemLike | null) {
    super((problem && (problem.detail || problem.title)) || ('HTTP ' + status));
    this.name = 'ApiError';
    this.status = status;
    this.code = problem?.code;
    this.fields = fieldProblems(problem?.errors);
    this.traceId = typeof problem?.trace_id === 'string' && /^[0-9a-f]{8,64}$/.test(problem.trace_id) ? problem.trace_id : undefined;
  }
}

// Thân lỗi đến từ mạng: chỉ nhận những mục đúng dạng, và có trần số mục.
const MAX_FIELD_PROBLEMS = 100;

function fieldProblems(raw: unknown): FieldProblem[] {
  if (!Array.isArray(raw)) return [];
  const out: FieldProblem[] = [];
  for (const item of raw.slice(0, MAX_FIELD_PROBLEMS)) {
    if (!item || typeof item !== 'object') continue;
    const { field, code, message } = item as Record<string, unknown>;
    if (typeof field !== 'string' || typeof message !== 'string') continue;
    out.push({ field, message, code: typeof code === 'string' ? code : undefined });
  }
  return out;
}

// fieldErrors: lỗi theo trường của một lỗi bất kỳ, dạng { tên trường: thông báo }. Trường
// có nhiều lỗi thì nối các thông báo. Lỗi không phải ApiError cho bản đồ rỗng.
export function fieldErrors(error: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (!(error instanceof ApiError)) return out;
  for (const f of error.fields) {
    out[f.field] = Object.hasOwn(out, f.field) ? out[f.field] + ' ' + f.message : f.message;
  }
  return out;
}

// isStale: bản đang sửa đã bị thay đổi ở nơi khác kể từ lúc đọc (ETag lệch, 412). KHÔNG gồm
// 409: máy chủ dùng 409 cho xung đột nghiệp vụ (slug đã dùng, tài nguyên đang được dùng…)
// và thông báo của nó phải hiện nguyên văn.
export function isStale(error: unknown): boolean {
  return error instanceof ApiError && error.status === 412;
}
