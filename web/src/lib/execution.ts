// Những gì nhiều màn hình cần biết về một execution.

// Trạng thái kết thúc: execution ở các trạng thái này không còn đổi, màn hình thôi tự làm mới.
export const TERMINAL: ReadonlySet<string> = new Set(['completed', 'failed', 'cancelled', 'budget_exceeded', 'timed_out']);

// messageOf: trường `message` của input / output, nếu là chuỗi.
export function messageOf(value: unknown): string {
  if (!value || typeof value !== 'object') return '';
  const m = (value as Record<string, unknown>).message;
  return typeof m === 'string' ? m : '';
}
