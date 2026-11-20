import { describe, expect, it } from 'vitest';
import { layeredLayout, type LayoutEdge, type LayoutNode } from './graph-layout';

const node = (id: string, rank: number): LayoutNode => ({ id, rank, width: 100, height: 40 });

describe('layeredLayout', () => {
  it('đặt mỗi tầng thành một cột, từ trái sang phải', () => {
    const at = layeredLayout([node('a', 0), node('b', 1), node('c', 2)], [{ source: 'a', target: 'b' }, { source: 'b', target: 'c' }], { columnGap: 50 });
    expect(at.get('a')?.x).toBe(0);
    expect(at.get('b')?.x).toBe(150);
    expect(at.get('c')?.x).toBe(300);
  });

  it('bỏ qua tầng không có node nào (không để cột trống)', () => {
    const at = layeredLayout([node('a', 0), node('z', 4)], [], { columnGap: 50 });
    expect(at.get('z')?.x).toBe(150);
  });

  it('không có hai node nào trong cùng một tầng chồng lên nhau', () => {
    const nodes = [node('root', 0), ...Array.from({ length: 30 }, (_, i) => node('n' + i, 1 + (i % 3)))];
    const edges: LayoutEdge[] = nodes.slice(1).map((n, i) => ({ source: i < 3 ? 'root' : 'n' + (i - 3), target: n.id }));
    const at = layeredLayout(nodes, edges, { rowGap: 10 });
    for (const rank of [1, 2, 3]) {
      const ys = nodes.filter(n => n.rank === rank).map(n => at.get(n.id)!.y).sort((a, b) => a - b);
      for (let i = 1; i < ys.length; i++) expect(ys[i]! - ys[i - 1]!).toBeGreaterThanOrEqual(50);
    }
  });

  it('xếp lại thứ tự trong tầng để hai cạnh song song không cắt nhau', () => {
    // a1 → b2 và a2 → b1: thứ tự ban đầu của tầng 1 làm hai cạnh cắt nhau.
    const at = layeredLayout([node('a1', 0), node('a2', 0), node('b1', 1), node('b2', 1)], [{ source: 'a1', target: 'b2' }, { source: 'a2', target: 'b1' }]);
    expect(Math.sign(at.get('a1')!.y - at.get('a2')!.y)).toBe(Math.sign(at.get('b2')!.y - at.get('b1')!.y));
  });

  it('node cha đứng giữa các node con của nó', () => {
    const at = layeredLayout([node('p', 0), node('c1', 1), node('c2', 1), node('c3', 1)], ['c1', 'c2', 'c3'].map(target => ({ source: 'p', target })));
    expect(at.get('p')!.y).toBe(at.get('c2')!.y);
  });

  it('cho cùng một kết quả ở hai lần gọi, và bỏ qua cạnh trỏ tới node không có', () => {
    const nodes = [node('a', 0), node('b', 1), node('c', 1)];
    const edges = [{ source: 'a', target: 'b' }, { source: 'a', target: 'missing' }, { source: 'b', target: 'c' }];
    expect([...layeredLayout(nodes, edges)]).toEqual([...layeredLayout(nodes, edges)]);
  });
});
