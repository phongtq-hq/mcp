import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { useConfirm, type ConfirmRequest } from './confirm';

afterEach(cleanup);

let ask: (request: ConfirmRequest) => Promise<boolean> = () => Promise.resolve(false);

function Host() {
  const [dialog, confirm] = useConfirm();
  ask = confirm;
  return <div>{dialog}</div>;
}

// Trả về trong một object: `await` một hàm async trả về promise sẽ chờ luôn promise đó.
async function open(request: ConfirmRequest): Promise<{ answer: Promise<boolean> }> {
  let answer!: Promise<boolean>;
  await act(async () => { answer = ask(request); });
  return { answer };
}

describe('useConfirm', () => {
  it('không hiện gì cho tới khi được hỏi; xác nhận cho true và đóng hộp', async () => {
    render(<Host />);
    expect(screen.queryByRole('dialog')).toBeNull();
    const { answer } = await open({ title: 'Xoá budget?', body: 'Budget "ops"', confirmLabel: 'Xoá', danger: true });
    const dialog = screen.getByRole('dialog');
    expect(document.getElementById(dialog.getAttribute('aria-labelledby')!)!.textContent).toBe('Xoá budget?');
    expect(screen.getByText('Budget "ops"')).toBeTruthy();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Xoá' })); });
    await expect(answer).resolves.toBe(true);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('Huỷ, Esc và rời màn hình đều cho false', async () => {
    const view = render(<Host />);
    let { answer } = await open({ title: 'A' });
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Huỷ' })); });
    await expect(answer).resolves.toBe(false);

    ({ answer } = await open({ title: 'B' }));
    await act(async () => { fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true })); });
    await expect(answer).resolves.toBe(false);

    ({ answer } = await open({ title: 'C' }));
    view.unmount();
    await expect(answer).resolves.toBe(false);
  });

  it('câu hỏi mới thay câu hỏi cũ: câu cũ nhận false, không treo', async () => {
    render(<Host />);
    const { answer: first } = await open({ title: 'Cũ' });
    const { answer: second } = await open({ title: 'Mới' });
    await expect(first).resolves.toBe(false);
    expect(screen.getByText('Mới')).toBeTruthy();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Xác nhận' })); });
    await expect(second).resolves.toBe(true);
  });

  it('ký tự ẩn trong tên do tenant đặt được làm lộ', async () => {
    render(<Host />);
    await open({ title: 'Xoá “ops\u202Egnp.exe”?' });
    expect(screen.getByRole('heading').textContent).toBe('Xoá “ops\\u202Egnp.exe”?');
  });

  it('đóng hộp bằng đường khác (sự kiện close) cũng là huỷ', async () => {
    render(<Host />);
    const { answer } = await open({ title: 'A' });
    await act(async () => { fireEvent(screen.getByRole('dialog'), new Event('close')); });
    await expect(answer).resolves.toBe(false);
  });

  it('nút Huỷ đứng trước nút hành động: Enter vô ý không kích hoạt', async () => {
    render(<Host />);
    await open({ title: 'Thu hồi?', confirmLabel: 'Thu hồi', danger: true });
    const buttons = screen.getAllByRole('button').map(b => b.textContent);
    expect(buttons).toEqual(['Huỷ', 'Thu hồi']);
  });
});
