import { useTween } from '@/lib/use-tween';

// Count: một số liệu đi dần tới giá trị mới. `format` nhận giá trị đang hiện; khi đã tới đích
// nó nhận đúng `value`, nên con số cuối cùng trên màn hình luôn là con số của API.
export function Count({ value, format }: { value: number; format: (n: number) => string }) {
  const shown = useTween(value);
  return <>{format(shown === value ? value : shown)}</>;
}
