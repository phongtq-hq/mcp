#!/bin/sh
# Sao lưu logic PostgreSQL vào /backups (thư mục HQ_BACKUP_DIR của máy chủ). Mỗi lần chạy
# một thư mục theo giờ UTC; xong hẳn mới đổi tên khỏi ".partial", nên thư mục không có đuôi
# đó luôn là một bản đầy đủ. Xoá bản cũ hơn HQ_BACKUP_KEEP_DAYS ngày.
#
# Đây là bản sao trên CÙNG máy chủ: phải chép /backups ra nơi khác (và diễn tập khôi phục)
# thì mới tính là có sao lưu. Xem runbook.
set -eu
umask 077

stamp=$(date -u +%Y%m%dT%H%M%SZ)
out=/backups/$stamp
mkdir -p "$out.partial"

# Role và mật khẩu (dạng hash) của cả máy chủ database.
pg_dumpall --globals-only -f "$out.partial/globals.sql"

for db in "${HQ_DB_NAME:-hq}" keycloak temporal temporal_visibility; do
  if [ "$(psql -X -At -d postgres -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" = 1 ]; then
    echo "backup: $db"
    pg_dump -Fc -d "$db" -f "$out.partial/$db.dump"
  fi
done

mv "$out.partial" "$out"
echo "backup: xong $out ($(du -sh "$out" | cut -f1))"

keep=${HQ_BACKUP_KEEP_DAYS:-14}
find /backups -mindepth 1 -maxdepth 1 -type d -mtime "+$keep" -exec rm -rf {} +
