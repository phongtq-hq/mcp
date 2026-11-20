// Định tuyến model theo tier (ADR-0010): khoá `model_routing` trong settings của organization
// hoặc project. Mỗi tier là một chuỗi có thứ tự: model chính rồi tối đa ba model dự phòng.
// Ghi settings là thay cả object, nên các khoá khác được gửi lại nguyên vẹn từ bản vừa đọc.
import { useState } from 'react';
import { t, type Key } from '@/i18n';
import { Form, SelectField, type Option } from './form';
import { useSubmit } from './use-submit';

const TIERS = ['economy', 'standard', 'premium'] as const;
type Tier = typeof TIERS[number];
const SLOTS = 4;
const TIER_LABEL: Record<Tier, Key> = { economy: 'tier_economy', standard: 'tier_standard', premium: 'tier_premium' };

function asObject(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

// chains đọc chuỗi model đang lưu của từng tier; dữ liệu sai dạng coi như chưa đặt.
function chains(settings: Record<string, unknown>): Record<Tier, string[]> {
  const tiers = asObject(asObject(settings.model_routing).tiers);
  const out = {} as Record<Tier, string[]>;
  for (const tier of TIERS) {
    const models: unknown = asObject(tiers[tier]).models;
    const ids = Array.isArray(models) ? models.filter((m): m is string => typeof m === 'string').slice(0, SLOTS) : [];
    out[tier] = Array.from({ length: SLOTS }, (_, i) => ids[i] ?? '');
  }
  return out;
}

interface Props {
  settings: unknown;
  // models: các model chọn được; null = không đọc được danh sách (chỉ giữ / bỏ được model đang lưu).
  models: readonly Option[] | null;
  // truncated: danh sách model còn trang sau chưa tải; model ngoài trang đầu không chọn được ở đây.
  truncated?: boolean;
  save: (settings: Record<string, unknown>) => Promise<unknown>;
  onChange: () => void;
  onSaved: () => void | Promise<void>;
}

export function RoutingForm({ settings, models, truncated, save, onChange, onSaved }: Props) {
  const current = asObject(settings);
  const [state, setState] = useState(() => chains(current));

  const submit = useSubmit(() => {
    const tiers: Record<string, { models: string[] }> = {};
    for (const tier of TIERS) {
      const ids = state[tier].filter(Boolean);
      if (ids.length > 0) tiers[tier] = { models: ids };
    }
    const next: Record<string, unknown> = { ...current };
    // Không tier nào có model: bỏ hẳn khoá (project khi đó dùng cấu hình của organization).
    if (Object.keys(tiers).length > 0) next.model_routing = { ...asObject(current.model_routing), tiers };
    else delete next.model_routing;
    return save(next);
  }, onSaved);

  const set = (tier: Tier, slot: number, value: string) => {
    setState(prev => ({ ...prev, [tier]: prev[tier].map((v, i) => (i === slot ? value : v)) }));
    onChange();
  };

  const known = models ?? [];
  return (
    <Form submit={submit}>
      {models === null ? <p className="muted">{t('routingNoModels')}</p> : null}
      {truncated ? <p className="muted">{t('listTruncated')}</p> : null}
      {TIERS.map(tier => (
        <fieldset className="plain tier" key={tier}>
          <legend>{t(TIER_LABEL[tier])}</legend>
          <div className="grid">
            {state[tier].map((value, slot) => {
              // Model đang lưu mà không còn trong danh sách vẫn hiện bằng id: lưu lại không lặng lẽ bỏ nó.
              const options = value && !known.some(o => o.value === value) ? [{ value, label: value }, ...known] : known;
              return (
                <SelectField
                  key={slot}
                  label={slot === 0 ? t('routingPrimary') : t('routingFallback', { n: slot })}
                  value={value}
                  onChange={v => set(tier, slot, v)}
                  placeholder={t('none')}
                  options={options}
                />
              );
            })}
          </div>
        </fieldset>
      ))}
    </Form>
  );
}
