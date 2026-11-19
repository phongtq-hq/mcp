// Lựa chọn hiển thị của người dùng (bộ lọc, khoảng thời gian) giữ trong sessionStorage.
// Trình duyệt có thể chặn storage: khi đó dùng mặc định và bỏ qua việc ghi.
export function stored(key: string, fallback: string): string {
  try { return sessionStorage.getItem(key) || fallback; } catch { return fallback; }
}

export function store(key: string, value: string): void {
  try { sessionStorage.setItem(key, value); } catch { /* bỏ qua */ }
}
