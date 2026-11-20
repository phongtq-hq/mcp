// Ô soạn toàn bộ `settings` của organization / project dưới dạng JSON. settings là object
// tự do và mỗi lần ghi THAY cả object, nên form này luôn bắt đầu từ bản vừa đọc và gửi lại
// kèm ETag của nó; cấu trúc từng khoá do máy chủ kiểm (422).
import { useState } from 'react';
import { t } from '@/i18n';
import { parseJsonObject, pretty } from '@/lib/json';
import { Form, JsonField } from './form';
import { useSubmit } from './use-submit';

interface Props {
  settings: unknown;
  // save ghi cả object settings; ném lỗi của API khi thất bại.
  save: (settings: Record<string, unknown>) => Promise<unknown>;
  onSaved: () => void | Promise<void>;
  onChange?: () => void;
}

export function SettingsForm({ settings, save, onSaved, onChange }: Props) {
  const [text, setText] = useState(() => pretty(settings));
  const parsed = parseJsonObject(text);
  const submit = useSubmit(async () => {
    if (!parsed.ok) throw new Error(t('settingsMustBeObject'));
    return save(parsed.value);
  }, onSaved);
  return (
    <Form submit={submit} disabled={!parsed.ok}>
      <JsonField
        label={t('settingsJson')}
        hint={t('advancedSettingsHint')}
        value={text}
        onChange={v => { setText(v); onChange?.(); }}
        error={submit.fields.settings ?? (parsed.ok ? undefined : t('settingsMustBeObject'))}
      />
    </Form>
  );
}
