import { useCallback, useState } from 'react';
import { store, stored } from './storage';

// useStored: một lựa chọn hiển thị (bộ lọc, khoảng thời gian) giữ qua các lần mở trang
// trong cùng phiên trình duyệt. Chỉ dùng cho lựa chọn hiển thị, không cho dữ liệu của tenant.
export function useStored(key: string, fallback: string): [string, (value: string) => void] {
  const [state, setState] = useState(() => ({ key, value: stored(key, fallback) }));
  // Khoá đổi (component được dùng lại cho dự án khác): đọc giá trị của khoá mới ngay trong
  // lượt dựng này, không giữ giá trị của khoá cũ.
  const current = state.key === key ? state : { key, value: stored(key, fallback) };
  if (current !== state) setState(current);
  const set = useCallback((next: string) => { store(key, next); setState({ key, value: next }); }, [key]);
  return [current.value, set];
}
