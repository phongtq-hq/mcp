import { useEffect, useState } from 'react';

// useDebounced: giá trị chỉ đổi sau khi người dùng ngừng gõ `ms` mili giây, để ô tìm kiếm
// không gửi một request cho mỗi phím.
export function useDebounced<T>(value: T, ms = 300): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const handle = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(handle);
  }, [value, ms]);
  return settled;
}
