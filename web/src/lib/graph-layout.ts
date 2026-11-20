// Bố cục đồ thị theo tầng, thuần (không DOM, không thư viện). Mỗi node đã biết tầng của nó
// (rank) từ loại node, nên phần khó của một bộ bố cục tổng quát (gán tầng) không cần tới:
// việc còn lại là xếp thứ tự trong từng tầng để giảm đường cắt nhau (trung bình vị trí hàng
// xóm, quét xuôi rồi ngược) và đặt toạ độ sao cho node đứng gần hàng xóm của nó. Đã cân nhắc
// Dagre và ELK (xem .docs/frontend-audit.md): với đồ thị đã có tầng, chúng không thêm gì đáng
// một phụ thuộc lúc chạy.
export interface LayoutNode { id: string; rank: number; width: number; height: number }
export interface LayoutEdge { source: string; target: string }
export interface Point { x: number; y: number }
export interface LayoutOptions { columnGap?: number; rowGap?: number; sweeps?: number }

const mean = (values: number[]): number => values.reduce((a, b) => a + b, 0) / values.length;

export function layeredLayout(nodes: readonly LayoutNode[], edges: readonly LayoutEdge[], options: LayoutOptions = {}): Map<string, Point> {
  const columnGap = options.columnGap ?? 120;
  const rowGap = options.rowGap ?? 18;
  const sweeps = options.sweeps ?? 4;
  const byId = new Map(nodes.map(n => [n.id, n]));

  // Hàng xóm ở tầng thấp hơn (lower) và cao hơn (upper). Cạnh trong cùng một tầng (Agent giao
  // việc cho Agent) không tham gia bố cục.
  const lower = new Map<string, string[]>();
  const upper = new Map<string, string[]>();
  for (const n of nodes) { lower.set(n.id, []); upper.set(n.id, []); }
  for (const e of edges) {
    const a = byId.get(e.source);
    const b = byId.get(e.target);
    if (!a || !b || a.rank === b.rank) continue;
    const [lo, hi] = a.rank < b.rank ? [a, b] : [b, a];
    upper.get(lo.id)?.push(hi.id);
    lower.get(hi.id)?.push(lo.id);
  }

  const ranks = [...new Set(nodes.map(n => n.rank))].sort((a, b) => a - b);
  const columns = ranks.map(r => nodes.filter(n => n.rank === r));

  // Thứ tự trong tầng: mỗi node về vị trí trung bình của hàng xóm ở phía đang quét. Node không
  // có hàng xóm ở phía đó giữ chỗ của nó. Sắp xếp ổn định nên kết quả không đổi giữa hai lần gọi.
  const index = new Map<string, number>();
  const reindex = (column: LayoutNode[]) => column.forEach((n, i) => index.set(n.id, i));
  columns.forEach(reindex);
  const order = (column: LayoutNode[], side: Map<string, string[]>) => {
    const key = new Map(column.map((n, i) => {
      const around = (side.get(n.id) ?? []).map(id => index.get(id) ?? 0);
      return [n.id, around.length ? mean(around) : i] as const;
    }));
    column.sort((a, b) => (key.get(a.id) ?? 0) - (key.get(b.id) ?? 0));
    reindex(column);
  };
  for (let s = 0; s < sweeps; s++) {
    for (let c = 1; c < columns.length; c++) order(columns[c]!, lower);
    for (let c = columns.length - 2; c >= 0; c--) order(columns[c]!, upper);
  }

  // Toạ độ dọc: đặt từng tầng theo thứ tự đã xếp, mỗi node ở gần tâm của hàng xóm nhất có thể
  // mà không chồng lên node trước nó; rồi dịch cả tầng để sai lệch trung bình bằng 0.
  const centre = new Map<string, number>();
  const place = (column: LayoutNode[], side: Map<string, string[]>) => {
    let cursor = 0;
    const drift: number[] = [];
    const top = new Map<string, number>();
    for (const n of column) {
      const around = (side.get(n.id) ?? []).map(id => centre.get(id)).filter((v): v is number => v !== undefined);
      // Không có hàng xóm ở phía này: giữ chỗ của lượt đặt trước (nếu có).
      const prior = centre.get(n.id);
      const wanted = around.length ? mean(around) - n.height / 2 : prior !== undefined ? prior - n.height / 2 : cursor;
      const y = Math.max(wanted, cursor);
      if (around.length) drift.push(wanted - y);
      top.set(n.id, y);
      cursor = y + n.height + rowGap;
    }
    const shift = drift.length ? mean(drift) : 0;
    for (const n of column) centre.set(n.id, (top.get(n.id) ?? 0) + shift + n.height / 2);
  };
  columns.forEach(column => place(column, lower));
  for (let c = columns.length - 2; c >= 0; c--) place(columns[c]!, upper);

  const out = new Map<string, Point>();
  let x = 0;
  for (const column of columns) {
    for (const n of column) out.set(n.id, { x, y: Math.round((centre.get(n.id) ?? 0) - n.height / 2) });
    x += Math.max(...column.map(n => n.width)) + columnGap;
  }
  return out;
}
