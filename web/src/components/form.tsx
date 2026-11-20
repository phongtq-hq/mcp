// Thành phần form dùng chung. Mỗi ô có nhãn thật (label bao ô nhập), gợi ý và lỗi được nối
// với ô bằng aria-describedby; lỗi theo trường là lỗi API trả về (useSubmit), không phải
// phỏng đoán của trình duyệt.
import { useId, type FormEvent, type ReactNode } from 'react';
import { t } from '@/i18n';
import { isStale } from '@/lib/api-error';
import { parseJson } from '@/lib/json';
import type { Submit } from './use-submit';
import { ErrorBox, Notice } from './ui';

interface FieldProps { label: string; hint?: ReactNode; error?: string | undefined; required?: boolean }

// Field: nhãn, ô nhập, gợi ý và lỗi. <label> chỉ chứa nhãn (tên của ô không đổi khi lỗi
// xuất hiện); gợi ý và lỗi nối với ô bằng aria-describedby.
function Field({ label, hint, error, required, id, children }: FieldProps & { id: string; children: ReactNode }) {
  return (
    <div className={error ? 'field invalid' : 'field'}>
      <div className="field-label">
        <label htmlFor={id}>{label}</label>
        {required ? <span aria-hidden="true"> *</span> : null}
      </div>
      {children}
      {hint ? <span className="hint" id={id + '-hint'}>{hint}</span> : null}
      {error ? <span className="field-error" id={id + '-error'} role="alert">{error}</span> : null}
    </div>
  );
}

function describe(id: string, hint: ReactNode, error: string | undefined): string | undefined {
  const ids = [hint ? id + '-hint' : '', error ? id + '-error' : ''].filter(Boolean).join(' ');
  return ids || undefined;
}

interface TextProps extends FieldProps {
  value: string;
  onChange: (value: string) => void;
  type?: 'text' | 'number' | 'url' | 'password' | 'datetime-local';
  placeholder?: string;
  disabled?: boolean;
  maxLength?: number;
  // Ô nhập secret dùng "new-password": trình duyệt bỏ qua "off" trên ô password, còn
  // "new-password" ngăn nó tự điền mật khẩu đã lưu của origin vào đây. Trình quản lý mật khẩu
  // vẫn có thể mời lưu giá trị vừa gõ; đó là quyết định của người dùng, giao diện không chặn được.
  autoComplete?: string;
  mono?: boolean;
}

export function TextField({ label, hint, error, required, value, onChange, type = 'text', mono, ...rest }: TextProps) {
  const id = useId();
  return (
    <Field label={label} hint={hint} error={error} required={required} id={id}>
      <input
        {...rest}
        id={id}
        type={type}
        className={mono ? 'mono' : undefined}
        value={value}
        required={required}
        aria-invalid={error ? true : undefined}
        aria-describedby={describe(id, hint, error)}
        onChange={e => onChange(e.target.value)}
      />
    </Field>
  );
}

interface AreaProps extends FieldProps {
  value: string;
  onChange: (value: string) => void;
  rows?: number;
  placeholder?: string;
  disabled?: boolean;
  mono?: boolean;
}

export function TextAreaField({ label, hint, error, required, value, onChange, rows = 4, mono, ...rest }: AreaProps) {
  const id = useId();
  return (
    <Field label={label} hint={hint} error={error} required={required} id={id}>
      <textarea
        {...rest}
        id={id}
        rows={rows}
        className={mono ? 'mono' : undefined}
        value={value}
        required={required}
        spellCheck={mono ? false : undefined}
        aria-invalid={error ? true : undefined}
        aria-describedby={describe(id, hint, error)}
        onChange={e => onChange(e.target.value)}
      />
    </Field>
  );
}

// JsonField: ô soạn JSON. Cú pháp sai được báo ngay dưới ô; cấu trúc do API kiểm.
export function JsonField({ error, value, ...rest }: Omit<AreaProps, 'mono'>) {
  const syntax = value.trim() !== '' && !parseJson(value).ok ? t('invalidJson') : undefined;
  return <TextAreaField rows={10} {...rest} value={value} mono error={error ?? syntax} />;
}

export interface Option { value: string; label: string }

interface SelectProps extends FieldProps {
  value: string;
  onChange: (value: string) => void;
  options: readonly Option[];
  // placeholder: mục rỗng đầu danh sách ("— chọn —"); không có thì phải chọn một giá trị.
  placeholder?: string;
  disabled?: boolean;
}

export function SelectField({ label, hint, error, required, value, onChange, options, placeholder, disabled }: SelectProps) {
  const id = useId();
  return (
    <Field label={label} hint={hint} error={error} required={required} id={id}>
      <select
        id={id}
        value={value}
        required={required}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
        aria-describedby={describe(id, hint, error)}
        onChange={e => onChange(e.target.value)}
      >
        {placeholder !== undefined ? <option value="">{placeholder}</option> : null}
        {options.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
      </select>
    </Field>
  );
}

export function CheckField({ label, hint, checked, onChange, disabled }: { label: string; hint?: ReactNode; checked: boolean; onChange: (checked: boolean) => void; disabled?: boolean }) {
  const id = useId();
  return (
    <div className="check">
      <input id={id} type="checkbox" checked={checked} disabled={disabled} aria-describedby={hint ? id + '-hint' : undefined} onChange={e => onChange(e.target.checked)} />
      <div>
        <label htmlFor={id}>{label}</label>
        {hint ? <span className="hint" id={id + '-hint'}>{hint}</span> : null}
      </div>
    </div>
  );
}

// SubmitError: lỗi của lần gửi gần nhất, nguyên văn của API. Riêng 412 (bản đang sửa đã đổi
// ở nơi khác) có thông báo riêng: người dùng phải tải lại rồi mới sửa tiếp, không gửi đè.
export function SubmitError({ submit }: { submit: Pick<Submit, 'error'> }) {
  if (!submit.error) return null;
  if (isStale(submit.error)) return <Notice kind="warn">{t('errConflict')}</Notice>;
  return <ErrorBox error={submit.error} />;
}

interface FormProps {
  submit: Submit;
  // submitLabel: nhãn nút gửi; trong lúc gửi nút bị khoá và hiện "Đang lưu…".
  submitLabel?: string;
  onCancel?: () => void;
  // disabled: form chưa gửi được (ví dụ JSON sai cú pháp).
  disabled?: boolean;
  danger?: boolean;
  children: ReactNode;
}

export function Form({ submit, submitLabel, onCancel, disabled, danger, children }: FormProps) {
  const onSubmit = (e: FormEvent) => { e.preventDefault(); void submit.run(); };
  return (
    <form className="form" onSubmit={onSubmit} noValidate={false}>
      {children}
      <SubmitError submit={submit} />
      <div className="row">
        <button className={danger ? 'danger' : 'primary'} type="submit" disabled={submit.busy || disabled}>
          {submit.busy ? t('saving') : (submitLabel ?? t('save'))}
        </button>
        {onCancel ? <button type="button" onClick={onCancel} disabled={submit.busy}>{t('cancel')}</button> : null}
      </div>
    </form>
  );
}
