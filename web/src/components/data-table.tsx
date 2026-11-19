import type { ReactNode } from 'react';
import { t } from '@/i18n';
import { Empty } from './ui';

export interface Column<T> {
  label: string;
  cell: (row: T) => ReactNode;
  className?: string;
}

interface Props<T> {
  columns: Column<T>[];
  rows: T[] | null | undefined;
  rowKey: (row: T) => string;
  // onRow: cả dòng bấm được (chuột và phím Enter).
  onRow?: (row: T) => void;
  emptyText?: string;
}

export function DataTable<T>({ columns, rows, rowKey, onRow, emptyText }: Props<T>) {
  if (!rows || rows.length === 0) {
    return <div className="table-wrap"><Empty>{emptyText || t('noData')}</Empty></div>;
  }
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>{columns.map((c, i) => <th key={i} className={c.className}>{c.label}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map(r => (
            <tr
              key={rowKey(r)}
              className={onRow ? 'link' : undefined}
              tabIndex={onRow ? 0 : undefined}
              onClick={onRow ? () => onRow(r) : undefined}
              onKeyDown={onRow ? e => { if (e.key === 'Enter' && e.target === e.currentTarget) onRow(r); } : undefined}
            >
              {columns.map((c, i) => <td key={i} className={c.className}>{c.cell(r)}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
