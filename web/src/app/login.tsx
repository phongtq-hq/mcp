import * as auth from '@/auth/oidc';
import { Notice } from '@/components/ui';
import { lang, languages, setLang, t } from '@/i18n';

// Thông báo ở màn hình đăng nhập chỉ lấy từ từ điển theo một mã cố định: văn bản lỗi IdP
// đưa về trên URL không bao giờ được hiển thị.
function noticeText(notice: auth.SignInNotice): string | null {
  if (notice === 'session') return t('errSession');
  if (notice === 'state') return t('loginStateMismatch');
  if (notice === 'failed') return t('loginFailed');
  return null;
}

export function LanguageSelect() {
  return (
    <label>
      {t('language')}
      <select value={lang()} onChange={e => setLang(e.target.value)}>
        {languages.map(([code, name]) => <option key={code} value={code}>{name}</option>)}
      </select>
    </label>
  );
}

export function Login({ notice }: { notice: auth.SignInNotice }) {
  const message = noticeText(notice);
  return (
    <div className="center">
      <div className="card login">
        <div className="brand-mark" aria-hidden="true">M</div>
        <h1>{t('loginTitle')}</h1>
        <p className="sub">{t('loginLead')}</p>
        {message ? <Notice kind="warn">{message}</Notice> : null}
        <button className="primary" onClick={() => { void auth.login(); }}>{t('loginButton')}</button>
        <LanguageSelect />
      </div>
    </div>
  );
}
