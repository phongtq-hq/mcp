import { useState } from 'react';

// useNewKeys: những khoá vừa xuất hiện so với lượt dựng trước (dòng mới sau một lần tự làm
// mới hoặc "tải thêm"). Lượt dựng đầu tiên không có khoá nào là mới: cả danh sách vừa hiện.
export function useNewKeys(keys: readonly string[]): ReadonlySet<string> {
  const [state, setState] = useState(() => ({ seen: new Set(keys), fresh: new Set<string>() }));
  const added = keys.filter(k => !state.seen.has(k));
  if (added.length === 0) return state.fresh;
  const seen = new Set(state.seen);
  for (const k of added) seen.add(k);
  const next = { seen, fresh: new Set(added) };
  setState(next);
  return next.fresh;
}
