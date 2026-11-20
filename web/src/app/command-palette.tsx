// Bảng lệnh (Ctrl / Cmd + K): nhảy tới một màn hình hoặc một dự án bằng bàn phím. Danh sách
// dự án đọc từ API khi bảng mở. API chưa có endpoint tìm kiếm chung (Agent, lần chạy, trace…),
// nên bảng lệnh chỉ tìm trong những gì nó liệt kê được; nó không giả vờ tìm thứ khác.
import { useQuery } from '@tanstack/react-query';
import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { useNavigate } from 'react-router';
import { get, soft, type Page, type Schemas } from '@/api/client';
import { Icon, type IconName } from '@/components/icon';
import { t } from '@/i18n';
import { NAV_ITEMS } from './navigation';

interface Command { id: string; label: string; hint: string; icon: IconName; to: string }

const MAX_SHOWN = 12;

function fold(s: string): string {
  return s.normalize('NFD').replace(/[̀-ͯ]/g, '').replace(/đ/gi, 'd').toLowerCase();
}

// Thân của bảng chỉ được dựng khi mở: mỗi lần mở là một ô tìm trống.
export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  return open ? <Palette onClose={onClose} /> : null;
}

function Palette({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const dialog = useRef<HTMLDialogElement>(null);
  const listId = useId();
  const [text, setText] = useState('');
  const [cursor, setCursor] = useState(0);

  const projects = useQuery({
    queryKey: ['projects', 'palette'],
    queryFn: () => soft(get<Page<Schemas['Project']>>('/projects?limit=200')),
  });

  useEffect(() => {
    const el = dialog.current;
    if (!el) return;
    if (typeof el.showModal === 'function') { if (!el.open) el.showModal(); } else el.setAttribute('open', '');
    return () => { if (typeof el.close === 'function' && el.open) el.close(); };
  }, []);

  const commands = useMemo<Command[]>(() => {
    const screens: Command[] = NAV_ITEMS.map(item => ({ id: 'nav:' + item.href, label: t(item.label), hint: t('paletteScreen'), icon: item.icon, to: item.href }));
    const list: Command[] = (projects.data?.data ?? []).map(p => ({ id: 'project:' + p.id, label: p.name, hint: t('paletteProject'), icon: 'folder' as const, to: '/projects/' + p.id }));
    return [...screens, ...list];
  }, [projects.data]);

  const shown = useMemo(() => {
    const q = fold(text.trim());
    return (q ? commands.filter(c => fold(c.label).includes(q)) : commands).slice(0, MAX_SHOWN);
  }, [commands, text]);

  const at = Math.min(cursor, Math.max(shown.length - 1, 0));
  const run = (c: Command | undefined) => {
    if (!c) return;
    onClose();
    navigate(c.to);
  };
  const onKey = (e: KeyboardEvent) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); setCursor(Math.min(at + 1, shown.length - 1)); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setCursor(Math.max(at - 1, 0)); }
    else if (e.key === 'Enter') { e.preventDefault(); run(shown[at]); }
  };

  return (
    <dialog ref={dialog} className="palette" aria-label={t('paletteTitle')}
      onCancel={e => { e.preventDefault(); onClose(); }}
      onClose={onClose}
      // Bấm ra ngoài hộp (vào nền mờ) thì đóng.
      onClick={e => { if (e.target === e.currentTarget) onClose(); }}>
      <div className="palette-box">
        <div className="palette-input">
          <Icon name="search" />
          <input
            autoFocus type="text" role="combobox" aria-expanded="true" aria-controls={listId} aria-autocomplete="list"
            aria-activedescendant={shown[at] ? listId + ':' + at : undefined}
            aria-label={t('paletteTitle')} placeholder={t('palettePlaceholder')}
            value={text} onChange={e => { setText(e.target.value); setCursor(0); }} onKeyDown={onKey}
          />
          <kbd>Esc</kbd>
        </div>
        <ul className="palette-list" id={listId} role="listbox" aria-label={t('paletteTitle')}>
          {shown.map((c, i) => (
            <li key={c.id} id={listId + ':' + i} role="option" aria-selected={i === at}
              onMouseEnter={() => setCursor(i)} onClick={() => run(c)}>
              <Icon name={c.icon} />
              <span className="palette-label">{c.label}</span>
              <span className="palette-hint">{c.hint}</span>
            </li>
          ))}
          {shown.length === 0 ? <li className="palette-empty" role="presentation">{t('paletteEmpty')}</li> : null}
        </ul>
      </div>
    </dialog>
  );
}
