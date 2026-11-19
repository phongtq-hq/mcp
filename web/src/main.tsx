// Giao diện quản trị MCP Platform. Điều hướng bằng hash; mỗi màn hình đọc dữ liệu từ
// Management API bằng token của người dùng.
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from '@/app/app';
import * as auth from '@/auth/oidc';
import { Notice, Spinner } from '@/components/ui';
import { t } from '@/i18n';
import '@/styles/app.css';

async function start(): Promise<void> {
  const el = document.getElementById('app');
  if (!el) return;
  // Lượt quay về từ IdP mang ?code=…: lấy nó ra và xoá khỏi thanh địa chỉ + lịch sử ngay,
  // trước mọi lời gọi mạng và trước khi React dựng gì.
  const returned = auth.takeCallback();
  const root = createRoot(el);
  root.render(<div className="center"><Spinner /></div>);
  try {
    await auth.loadConfig();
  } catch {
    root.render(<div className="center"><Notice kind="bad">{t('configError')}</Notice></div>);
    return;
  }
  const back = await auth.completeLogin(returned);
  if (back && !back.ok) auth.setNotice(back.reason === 'state' ? 'state' : 'failed');
  root.render(<StrictMode><App /></StrictMode>);
}

void start();
