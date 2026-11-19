// Đọc JSON do người dùng gõ vào một ô nhập (định nghĩa workflow, settings…). Kết quả là
// unknown: API mới là nơi kiểm cấu trúc; ở đây chỉ kiểm cú pháp để báo lỗi sớm.
export type Parsed = { ok: true; value: unknown } | { ok: false };

export function parseJson(text: string): Parsed {
  if (text.trim() === '') return { ok: false };
  try { return { ok: true, value: JSON.parse(text) as unknown }; } catch { return { ok: false }; }
}

// parseJsonObject: như parseJson nhưng chỉ nhận một object (không mảng, không null).
export function parseJsonObject(text: string): { ok: true; value: Record<string, unknown> } | { ok: false } {
  const parsed = parseJson(text);
  if (!parsed.ok || parsed.value === null || typeof parsed.value !== 'object' || Array.isArray(parsed.value)) return { ok: false };
  return { ok: true, value: parsed.value as Record<string, unknown> };
}

// pretty: giá trị thành JSON thụt lề để đưa vào ô soạn (KHÔNG qua visible(): đây là nội
// dung sẽ được gửi lại, không phải nội dung để đọc và quyết định).
export function pretty(value: unknown): string {
  try { return JSON.stringify(value ?? {}, null, 2); } catch { return '{}'; }
}
