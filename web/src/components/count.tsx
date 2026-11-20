import { fmtMoney, type MoneyLike } from '@/lib/format';
import { useTween } from '@/lib/use-tween';

// Count: một số liệu đi dần tới giá trị mới. `format` nhận giá trị đang hiện; khi đã tới đích
// nó nhận đúng `value`, nên con số cuối cùng trên màn hình luôn là con số của API.
export function Count({ value, format }: { value: number; format: (n: number) => string }) {
  const shown = useTween(value);
  return <>{format(shown === value ? value : shown)}</>;
}

// MoneyCount: như Count cho một số tiền. Trong lúc chuyển tiếp nó hiện giá trị trung gian;
// khi dừng, nó hiện đúng chuỗi thập phân của API (không qua phép tính nào ở trình duyệt).
export function MoneyCount({ money }: { money: MoneyLike | null | undefined }) {
  const target = money && money.amount !== null && money.amount !== undefined ? Number(money.amount) : NaN;
  const shown = useTween(Number.isFinite(target) ? target : 0);
  if (!money || !Number.isFinite(target) || shown === target) return <>{fmtMoney(money)}</>;
  return <>{fmtMoney({ amount: shown.toFixed(8), currency: money.currency })}</>;
}
