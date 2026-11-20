import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/lib/api-error';
import { LoadMore } from './load-more';

afterEach(cleanup);

describe('LoadMore', () => {
  it('hết trang thì không hiện gì', () => {
    const { container } = render(<LoadMore query={{ hasNextPage: false, isFetchingNextPage: false, fetchNextPage: vi.fn() }} />);
    expect(container.textContent).toBe('');
  });

  it('còn trang: bấm để tải trang kế tiếp; đang tải thì khoá nút', () => {
    const fetchNextPage = vi.fn();
    const view = render(<LoadMore query={{ hasNextPage: true, isFetchingNextPage: false, fetchNextPage }} />);
    fireEvent.click(screen.getByRole('button', { name: 'Tải thêm' }));
    expect(fetchNextPage).toHaveBeenCalledTimes(1);
    view.rerender(<LoadMore query={{ hasNextPage: true, isFetchingNextPage: true, fetchNextPage }} />);
    expect(screen.getByRole('button')).toHaveProperty('disabled', true);
  });

  it('tải trang kế tiếp lỗi: hiện lỗi và vẫn cho thử lại', () => {
    render(<LoadMore query={{ hasNextPage: true, isFetchingNextPage: false, fetchNextPage: vi.fn(), isFetchNextPageError: true, error: new ApiError(403, null) }} />);
    expect(screen.getByRole('alert')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Tải thêm' })).toBeTruthy();
  });
});
