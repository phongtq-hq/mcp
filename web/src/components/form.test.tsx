import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/lib/api-error';
import { Form, JsonField, SelectField, TextField } from './form';
import { useSubmit } from './use-submit';

afterEach(cleanup);

function Demo({ save, onDone }: { save: (name: string) => Promise<unknown>; onDone?: () => void }) {
  const [name, setName] = useState('');
  const [env, setEnv] = useState('');
  const [json, setJson] = useState('{}');
  const submit = useSubmit(() => save(name), onDone);
  return (
    <Form submit={submit} submitLabel="Tạo">
      <TextField label="Tên" required value={name} onChange={setName} error={submit.fields.name} hint="Hiện ở danh sách" />
      <SelectField label="Môi trường" value={env} onChange={setEnv} placeholder="— chọn —" options={[{ value: 'prod', label: 'Production' }]} error={submit.fields.environment} />
      <JsonField label="Cài đặt" value={json} onChange={setJson} />
    </Form>
  );
}

describe('Form + useSubmit', () => {
  it('gửi giá trị đã nhập và gọi onDone khi API thành công', async () => {
    const save = vi.fn().mockResolvedValue({ id: 'p1' });
    const onDone = vi.fn();
    render(<Demo save={save} onDone={onDone} />);
    fireEvent.change(screen.getByLabelText(/Tên/), { target: { value: 'Ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ id: 'p1' }));
    expect(save).toHaveBeenCalledWith('Ops');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('lỗi 422 hiện ở đúng ô, nối với ô bằng aria, và giữ nguyên nội dung đã nhập', async () => {
    const save = vi.fn().mockRejectedValue(new ApiError(422, { detail: 'dữ liệu không hợp lệ', errors: [{ field: 'name', message: 'tên đã tồn tại' }] }));
    const onDone = vi.fn();
    render(<Demo save={save} onDone={onDone} />);
    const input = screen.getByLabelText(/Tên/);
    fireEvent.change(input, { target: { value: 'Ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await screen.findByText('tên đã tồn tại');
    expect(input).toHaveProperty('value', 'Ops');
    expect(input.getAttribute('aria-invalid')).toBe('true');
    const described = input.getAttribute('aria-describedby')!.split(' ').map(id => document.getElementById(id)!.textContent);
    expect(described).toEqual(['Hiện ở danh sách', 'tên đã tồn tại']);
    expect(screen.getByText(/dữ liệu không hợp lệ/)).toBeTruthy();
    expect(onDone).not.toHaveBeenCalled();
  });

  it('412 hiện thông báo xung đột, không phải lỗi chung', async () => {
    render(<Demo save={vi.fn().mockRejectedValue(new ApiError(412, { detail: 'tài nguyên đã thay đổi' }))} />);
    fireEvent.change(screen.getByLabelText(/Tên/), { target: { value: 'Ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('đã bị thay đổi ở nơi khác'));
    expect(screen.queryByText(/tài nguyên đã thay đổi/)).toBeNull();
  });

  it('409 (xung đột nghiệp vụ) hiện nguyên thông báo của API, không bị đổi thành "đã thay đổi ở nơi khác"', async () => {
    render(<Demo save={vi.fn().mockRejectedValue(new ApiError(409, { detail: 'slug đã được dùng trong organization này', code: 'slug_taken' }))} />);
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: 'Ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    expect((await screen.findByRole('alert')).textContent).toContain('slug đã được dùng trong organization này');
    expect(screen.queryByText(/đã bị thay đổi ở nơi khác/)).toBeNull();
  });

  it('API đã thành công mà bước sau đó lỗi: không hiện lỗi gửi (không mời gửi lại việc đã xong)', async () => {
    const save = vi.fn().mockResolvedValue({ id: 'p1' });
    const onDone = vi.fn().mockRejectedValue(new Error('tải lại thất bại'));
    render(<Demo save={save} onDone={onDone} />);
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: 'Ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    await screen.findByRole('button', { name: 'Tạo' });
    expect(screen.queryByRole('alert')).toBeNull();
    expect(save).toHaveBeenCalledTimes(1);
  });

  it('tên của ô chỉ là nhãn: không gồm dấu *, gợi ý hay lỗi', async () => {
    render(<Demo save={vi.fn().mockRejectedValue(new ApiError(422, { errors: [{ field: 'name', message: 'tên đã tồn tại' }] }))} />);
    fireEvent.change(screen.getByLabelText('Tên'), { target: { value: 'Ops' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    await screen.findByText('tên đã tồn tại');
    expect(screen.getByRole('textbox', { name: 'Tên' })).toBeTruthy();
    expect(screen.getByRole('combobox', { name: 'Môi trường' })).toBeTruthy();
  });

  it('ô bắt buộc còn trống: trình duyệt chặn, không gọi API', () => {
    const save = vi.fn();
    render(<Demo save={save} />);
    fireEvent.click(screen.getByRole('button', { name: 'Tạo' }));
    expect(save).not.toHaveBeenCalled();
  });

  it('bấm hai lần trong lúc đang gửi chỉ gửi một lần; nút bị khoá', async () => {
    let finish: (v: unknown) => void = () => {};
    const save = vi.fn(() => new Promise(resolve => { finish = resolve; }));
    render(<Demo save={save} />);
    fireEvent.change(screen.getByLabelText(/Tên/), { target: { value: 'Ops' } });
    const button = screen.getByRole('button', { name: 'Tạo' });
    fireEvent.click(button);
    fireEvent.submit(button.closest('form')!);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Đang lưu…' })).toHaveProperty('disabled', true));
    expect(save).toHaveBeenCalledTimes(1);
    finish({});
    await screen.findByRole('button', { name: 'Tạo' });
  });

  it('JsonField báo cú pháp sai ngay dưới ô', () => {
    render(<Demo save={vi.fn()} />);
    const area = screen.getByLabelText(/Cài đặt/);
    fireEvent.change(area, { target: { value: '{sai' } });
    expect(screen.getByText('JSON không hợp lệ.')).toBeTruthy();
    fireEvent.change(area, { target: { value: '{"a":1}' } });
    expect(screen.queryByText('JSON không hợp lệ.')).toBeNull();
  });
});
