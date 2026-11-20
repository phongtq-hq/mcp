import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { Count, MoneyCount } from './count';
import { DataTable } from './data-table';
import { Skeleton } from './skeleton';
import { StatusBadge } from './ui';
import { durationMs, reducedMotion } from '@/lib/motion';

afterEach(cleanup);
const table = (ids: string[]) => <DataTable rows={ids.map(id => ({ id }))} rowKey={r => r.id} columns={[{ label: 'ID', cell: r => r.id }]} />;
const fresh = () => [...document.querySelectorAll('tr.row-new')].map(tr => tr.textContent);

describe('dòng mới của bảng', () => {
  it('lượt hiện đầu không dòng nào là mới; dòng xuất hiện sau đó được đánh dấu', () => {
    const view = render(table(['a', 'b']));
    expect(fresh()).toEqual([]);
    view.rerender(table(['c', 'a', 'b']));
    expect(fresh()).toEqual(['c']);
    // Lượt làm mới kế tiếp không có gì thêm: dấu của "c" giữ nguyên, không dòng cũ nào bị đánh dấu lại.
    view.rerender(table(['c', 'a', 'b']));
    expect(fresh()).toEqual(['c']);
    view.rerender(table(['d', 'c', 'a', 'b']));
    expect(fresh()).toEqual(['d']);
  });
  it('bảng rỗng rồi có dòng đầu tiên: dòng đó là mới', () => {
    const view = render(table([]));
    view.rerender(table(['a']));
    expect(fresh()).toEqual(['a']);
  });
});

describe('trạng thái và khung chờ', () => {
  it('huy hiệu trạng thái có hình riêng theo loại, không chỉ khác màu', () => {
    render(<><StatusBadge status="completed" /><StatusBadge status="failed" /><StatusBadge status="running" /><StatusBadge status="waiting_approval" /></>);
    const shapes = [...document.querySelectorAll('.badge svg')].map(svg => svg.firstElementChild?.getAttribute('d') ?? svg.firstElementChild?.tagName);
    expect(new Set(shapes).size).toBe(4);
    expect(document.querySelectorAll('.status-icon.spin')).toHaveLength(1);
    expect(screen.getByText('Hoàn thành')).toBeTruthy();
  });
  it('khung chờ báo trạng thái đang tải cho trình đọc màn hình', () => {
    render(<Skeleton kind="detail" />);
    expect(screen.getByRole('status', { name: 'Đang tải…' })).toBeTruthy();
  });
});

describe('token chuyển động', () => {
  it('không có matchMedia (môi trường test) thì coi như giảm chuyển động; thời lượng có giá trị dự phòng', () => {
    expect(reducedMotion()).toBe(true);
    expect(durationMs('fast')).toBeGreaterThan(0);
    expect(durationMs('flow')).toBeGreaterThan(durationMs('fast'));
  });
});

describe('số liệu chuyển tiếp', () => {
  it('giảm chuyển động: hiện ngay giá trị mới; số tiền luôn là đúng chuỗi của API', () => {
    const view = render(<><Count value={11} format={n => String(n)} /> | <MoneyCount money={{ amount: '0.00560000', currency: 'USD' }} /></>);
    expect(view.container.textContent).toContain('11');
    expect(view.container.textContent).toContain('0,0056');
    view.rerender(<><Count value={12} format={n => String(n)} /> | <MoneyCount money={{ amount: '0.00570000', currency: 'USD' }} /></>);
    expect(view.container.textContent).toContain('12');
    expect(view.container.textContent).toContain('0,0057');
    view.rerender(<MoneyCount money={null} />);
    expect(view.container.textContent).toBe('—');
  });
});
