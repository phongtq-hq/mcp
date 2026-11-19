import { QueryClient } from '@tanstack/react-query';

// Cấu hình truy vấn của cả ứng dụng. Các lựa chọn này là một phần của mô hình bảo mật của
// màn hình duyệt (security.md §4.1), không chỉ là tinh chỉnh: xem test ở
// approval-freshness.test.tsx trước khi đổi.
export function newQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Không tự thử lại và không tự tải lại khi cửa sổ được chọn hay mạng có lại: màn
        // hình chỉ đổi khi người dùng điều hướng hoặc khi chính nó xin tự làm mới
        // (refetchInterval). Nội dung của màn hình duyệt không được đổi dưới con trỏ của
        // người duyệt.
        retry: false,
        refetchOnWindowFocus: false,
        refetchOnReconnect: false,
        staleTime: 0,
      },
      mutations: { retry: false },
    },
  });
}
