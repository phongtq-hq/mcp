import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { useStored } from './use-stored';

let setValue: (v: string) => void = () => {};

function Probe({ k }: { k: string }) {
  const [value, set] = useStored(k, 'mặc định');
  setValue = set;
  return <div data-testid="v">{value}</div>;
}

beforeEach(() => sessionStorage.clear());
afterEach(cleanup);

describe('useStored', () => {
  it('dùng mặc định, ghi lại lựa chọn và đọc lại ở lần mở sau', () => {
    const first = render(<Probe k="cost.range" />);
    expect(screen.getByTestId('v').textContent).toBe('mặc định');
    act(() => setValue('30d'));
    expect(screen.getByTestId('v').textContent).toBe('30d');
    first.unmount();
    render(<Probe k="cost.range" />);
    expect(screen.getByTestId('v').textContent).toBe('30d');
  });

  it('khoá đổi thì đọc giá trị của khoá mới, không giữ giá trị của khoá cũ', () => {
    sessionStorage.setItem('filter.b', 'của b');
    const view = render(<Probe k="filter.a" />);
    act(() => setValue('của a'));
    view.rerender(<Probe k="filter.b" />);
    expect(screen.getByTestId('v').textContent).toBe('của b');
    view.rerender(<Probe k="filter.c" />);
    expect(screen.getByTestId('v').textContent).toBe('mặc định');
    view.rerender(<Probe k="filter.a" />);
    expect(screen.getByTestId('v').textContent).toBe('của a');
  });
});
