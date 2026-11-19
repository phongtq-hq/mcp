// Các ô dùng chung giữa form tạo và form sửa model.
import { CheckField, SelectField, TextField } from '@/components/form';
import { t, type Key } from '@/i18n';

const TIERS: readonly (readonly [string, Key])[] = [['economy', 'tier_economy'], ['standard', 'tier_standard'], ['premium', 'tier_premium']];

export interface ModelDraft {
  displayName: string;
  tier: string;
  toolCalling: boolean;
  structuredOutput: boolean;
  contextWindow: string;
  maxOutputTokens: string;
}

function asObject(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

export function draftOf(model?: { display_name: string; tier?: string | null; capabilities?: unknown }): ModelDraft {
  const caps = asObject(model?.capabilities);
  const num = (v: unknown) => (typeof v === 'number' ? String(v) : '');
  return {
    displayName: model?.display_name ?? '',
    tier: model?.tier ?? '',
    toolCalling: caps.tool_calling === true,
    structuredOutput: caps.structured_output === true,
    contextWindow: num(caps.context_window),
    maxOutputTokens: num(caps.max_output_tokens),
  };
}

// capabilitiesOf: capabilities gửi đi = bản đang lưu (giữ khoá mà form không biết) + các ô
// của form. Ô số để trống thì bỏ khoá đó.
export function capabilitiesOf(draft: ModelDraft, existing?: unknown): Record<string, unknown> {
  const caps: Record<string, unknown> = { ...asObject(existing), tool_calling: draft.toolCalling, structured_output: draft.structuredOutput };
  for (const [key, text] of [['context_window', draft.contextWindow], ['max_output_tokens', draft.maxOutputTokens]] as const) {
    if (text.trim() === '') delete caps[key]; else caps[key] = Number(text);
  }
  return caps;
}

interface Props {
  draft: ModelDraft;
  onChange: (draft: ModelDraft) => void;
  errors: Record<string, string>;
  // tierLocked: model đã có tier. API không gỡ được tier (nhận null thì giữ giá trị cũ), nên
  // form sửa không đưa ra lựa chọn "không có" mà nó không thực hiện được.
  tierLocked?: boolean;
}

export function ModelFields({ draft, onChange, errors, tierLocked }: Props) {
  const set = <K extends keyof ModelDraft>(key: K, value: ModelDraft[K]) => onChange({ ...draft, [key]: value });
  return (
    <>
      <TextField label={t('displayName')} required value={draft.displayName} onChange={v => set('displayName', v)} error={errors.display_name} maxLength={200} />
      <SelectField
        label={t('tier')}
        value={draft.tier}
        onChange={v => set('tier', v)}
        placeholder={tierLocked ? undefined : t('none')}
        options={TIERS.map(([value, label]) => ({ value, label: t(label) }))}
        error={errors.tier}
        hint={tierLocked ? t('tierLockedHint') : t('tierHint')}
      />
      <CheckField label={t('capToolCalling')} hint={t('capToolCallingHint')} checked={draft.toolCalling} onChange={v => set('toolCalling', v)} />
      <CheckField label={t('capStructuredOutput')} checked={draft.structuredOutput} onChange={v => set('structuredOutput', v)} />
      <TextField label={t('capContextWindow')} type="number" value={draft.contextWindow} onChange={v => set('contextWindow', v)} error={errors.capabilities} hint={t('capTokensHint')} />
      <TextField label={t('capMaxOutputTokens')} type="number" value={draft.maxOutputTokens} onChange={v => set('maxOutputTokens', v)} hint={t('capTokensHint')} />
    </>
  );
}
