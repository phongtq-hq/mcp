import { QueryClientProvider } from '@tanstack/react-query';
import { Fragment, useEffect, useState } from 'react';
import { HashRouter } from 'react-router';
import { useAuthState } from '@/auth/oidc';
import { useLang } from '@/i18n';
import { Login } from './login';
import { newQueryClient } from './query-client';
import { AppRoutes } from './routes';

export function App() {
  const lang = useLang();
  const { signedIn, notice } = useAuthState();
  const [client] = useState(newQueryClient);

  // Hết phiên: bỏ mọi dữ liệu đã tải. Người đăng nhập kế tiếp trên tab này không được
  // thấy dữ liệu của phiên trước.
  useEffect(() => {
    if (!signedIn) client.clear();
  }, [client, signedIn]);

  // key = ngôn ngữ: đổi ngôn ngữ dựng lại cả cây, mọi chuỗi và định dạng theo ngôn ngữ mới.
  return (
    <Fragment key={lang}>
      {signedIn ? (
        <QueryClientProvider client={client}>
          <HashRouter>
            <AppRoutes />
          </HashRouter>
        </QueryClientProvider>
      ) : <Login notice={notice} />}
    </Fragment>
  );
}
