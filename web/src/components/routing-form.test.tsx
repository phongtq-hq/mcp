import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/lib/api-error';
import { RoutingForm } from './routing-form';

afterEach(cleanup);

const MODELS = [{ value: 'm-sonnet', label: 'Sonnet' }, { value: 'm-haiku', label: 'Haiku' }];
const tier = (name: string) => within(screen.getByRole('group', { name }));

function show(settings: unknown, save = vi.fn().mockResolvedValue({}), models: typeof MODELS | null = MODELS) {
  const onChange = vi.fn();
  const onSaved = vi.fn();
  render(<RoutingForm settings={settings} models={models} save={save} onChange={onChange} onSaved={onSaved} />);
  return { save, onChange, onSaved };
}

describe('RoutingForm', () => {
  it('hiện chuỗi đang lưu theo đúng thứ tự', () => {
    show({ model_routing: { tiers: { standard: { models: ['m-sonnet', 'm-haiku'] } } } });
    expect(tier('Tier standard').getByLabelText('Model chính')).toHaveProperty('value', 'm-sonnet');
    expect(tier('Tier standard').getByLabelText('Dự phòng 1')).toHaveProperty('value', 'm-haiku');
    expect(tier('Tier standard').getByLabelText('Dự phòng 2')).toHaveProperty('value', '');
    expect(tier('Tier economy').getByLabelText('Model chính')).toHaveProperty('value', '');
  });

  it('lưu gửi cả settings: giữ các khoá khác, bỏ ô trống, giữ thứ tự, chỉ gồm tier có model', async () => {
    const { save, onChange, onSaved } = show({ orchestration: { planned_enabled: true }, model_routing: { tiers: { standard: { models: ['m-sonnet'] } } } });
    fireEvent.change(tier('Tier standard').getByLabelText('Dự phòng 2'), { target: { value: 'm-haiku' } });
    fireEvent.change(tier('Tier economy').getByLabelText('Model chính'), { target: { value: 'm-haiku' } });
    expect(onChange).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole('button', { name: 'Lưu' }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(save).toHaveBeenCalledWith({
      orchestration: { planned_enabled: true },
      model_routing: { tiers: { economy: { models: ['m-haiku'] }, standard: { models: ['m-sonnet', 'm-haiku'] } } },
    });
  });

  it('bỏ hết model của mọi tier: khoá model_routing bị gỡ, các khoá khác còn nguyên', async () => {
    const { save } = show({ a: 1, model_routing: { tiers: { premium: { models: ['m-sonnet'] } } } });
    fireEvent.change(tier('Tier premium').getByLabelText('Model chính'), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: 'Lưu' }));
    await waitFor(() => expect(save).toHaveBeenCalledWith({ a: 1 }));
  });

  it('model đang lưu không còn trong danh sách (đã tắt) vẫn hiện bằng id và được giữ khi lưu', async () => {
    const { save } = show({ model_routing: { tiers: { standard: { models: ['m-da-tat'] } } } });
    expect(tier('Tier standard').getByLabelText('Model chính')).toHaveProperty('value', 'm-da-tat');
    fireEvent.click(screen.getByRole('button', { name: 'Lưu' }));
    await waitFor(() => expect(save).toHaveBeenCalledWith({ model_routing: { tiers: { standard: { models: ['m-da-tat'] } } } }));
  });

  it('không đọc được danh sách model: báo rõ, vẫn giữ được chuỗi đang lưu', () => {
    show({ model_routing: { tiers: { standard: { models: ['m-sonnet'] } } } }, vi.fn(), null);
    expect(screen.getByText(/Không đọc được danh sách model/)).toBeTruthy();
    expect(tier('Tier standard').getByLabelText('Model chính')).toHaveProperty('value', 'm-sonnet');
  });

  it('settings đang lưu sai dạng không làm vỡ form', () => {
    show({ model_routing: { tiers: { standard: { models: 'không phải mảng' }, economy: 7 } } });
    expect(tier('Tier standard').getByLabelText('Model chính')).toHaveProperty('value', '');
    show(null);
  });

  it('máy chủ từ chối (422): hiện thông báo của API, không gọi onSaved', async () => {
    const save = vi.fn().mockRejectedValue(new ApiError(422, { detail: 'settings.model_routing.tiers.standard.models chứa model rỗng hoặc lặp lại', code: 'validation_failed' }));
    const { onSaved } = show({}, save);
    fireEvent.change(tier('Tier standard').getByLabelText('Model chính'), { target: { value: 'm-sonnet' } });
    fireEvent.change(tier('Tier standard').getByLabelText('Dự phòng 1'), { target: { value: 'm-sonnet' } });
    fireEvent.click(screen.getByRole('button', { name: 'Lưu' }));
    expect((await screen.findByRole('alert')).textContent).toContain('chứa model rỗng hoặc lặp lại');
    expect(onSaved).not.toHaveBeenCalled();
  });
});
