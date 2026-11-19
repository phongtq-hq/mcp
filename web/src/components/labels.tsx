// Nhãn của các giá trị liệt kê dùng ở nhiều màn hình.
import { t } from '@/i18n';
import { Badge } from './ui';

export function EnvLabel({ environment }: { environment: string }) {
  return environment === 'prod' ? <Badge tone="bad">{t('envProd')}</Badge> : <Badge>{t('envNonProd')}</Badge>;
}
