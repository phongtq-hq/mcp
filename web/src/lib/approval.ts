// Quy tắc hiển thị của một approval. Dữ liệu ở đây đến từ model và hệ thống ngoài: kiểu
// TypeScript chỉ là lời hứa của đặc tả, nên mọi trường vẫn được kiểm lúc chạy.
import { t, tOr } from '@/i18n';
import { visible } from './visible';

export interface ApprovalLike {
  subject_type: string;
  payload?: { tool_name?: unknown; arguments?: unknown; message?: unknown } | null;
}

// toolName: tên tool như Agent gọi nó. payload.tool_name đã là dạng <server>.<tool>.
export function toolName(a: ApprovalLike): string {
  const p = a.payload;
  return p && typeof p.tool_name === 'string' ? p.tool_name : '';
}

export function message(a: ApprovalLike): string {
  const p = a.payload;
  return p && typeof p.message === 'string' ? p.message : '';
}

// incomplete: approval không cho người duyệt thấy điều họ đang duyệt (thiếu tên tool
// hoặc tham số của một tool call, thiếu thông điệp của một cổng workflow). Khi đó giao
// diện không cho bấm Duyệt: duyệt một hành động không nhìn thấy không phải là duyệt.
export function incomplete(a: ApprovalLike): boolean {
  const p = a.payload || {};
  if (a.subject_type === 'tool_call') return !toolName(a) || p.arguments === undefined || p.arguments === null;
  if (a.subject_type === 'workflow_step') return !message(a).trim();
  return false;
}

export function subjectLabel(a: ApprovalLike): string {
  return tOr('subj_' + a.subject_type, a.subject_type);
}

export function subjectText(a: ApprovalLike): string {
  if (a.subject_type === 'tool_call') return toolName(a) || t('subj_tool_call');
  if (a.subject_type === 'workflow_step') return visible(message(a)) || t('subj_workflow_step');
  return subjectLabel(a);
}
