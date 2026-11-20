// Sau một lần triển khai, tên file của mọi phần trong bundle đổi theo nội dung. Tab mở từ
// trước vẫn giữ index.html cũ: lần đầu nó nạp một màn hình chưa tải, file cũ không còn và
// import() thất bại. Cách chữa là tải lại trang để lấy index.html mới.
import { store, stored } from './storage';

const KEY = 'hq.chunk-reload';
// Chỉ tự tải lại một lần trong khoảng này: nếu trang vừa tải lại mà vẫn lỗi thì nguyên nhân
// không phải bản build cũ, và tải lại nữa chỉ tạo vòng lặp.
const WINDOW_MS = 60_000;

// shouldReload: có nên tự tải lại trang sau một lần nạp màn hình thất bại không. Ghi nhận
// thời điểm khi trả lời "có".
export function shouldReload(now: number): boolean {
  const last = Number(stored(KEY, '0'));
  if (Number.isFinite(last) && last > 0 && now - last < WINDOW_MS) return false;
  store(KEY, String(now));
  // Storage bị chặn thì không nhớ được lần tải lại trước: không tự tải lại.
  return stored(KEY, '') === String(now);
}
