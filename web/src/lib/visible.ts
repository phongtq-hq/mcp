// visible làm lộ các ký tự không nhìn thấy trong văn bản không tin cậy (tham số tool do
// model sinh, thông điệp của cổng duyệt): ký tự điều khiển, ký tự đổi hướng chữ (RTL
// override…), ký tự rộng bằng không. Chúng có thể làm một dòng HIỂN THỊ khác với giá trị
// thật mà người duyệt sắp cho chạy; ở đây chúng hiện thành \uXXXX. Xuống dòng và tab giữ nguyên.
// eslint-disable-next-line no-control-regex -- mục đích của biểu thức này chính là bắt ký tự điều khiển
const HIDDEN = /[\u0000-\u0008\u000B-\u001F\u007F-\u009F\u00AD\u061C\u180E\u200B-\u200F\u2028-\u202F\u2060-\u206F\uFEFF\uFFF9-\uFFFB]/g;

export function visible(text: unknown): string {
  return String(text ?? '').replace(HIDDEN, c => '\\u' + c.charCodeAt(0).toString(16).toUpperCase().padStart(4, '0'));
}

// json: giá trị bất kỳ thành JSON thụt lề, đã qua visible.
export function json(value: unknown): string {
  try { return visible(JSON.stringify(value, null, 2)); } catch { return visible(String(value)); }
}
