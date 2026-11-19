#!/bin/sh
# Tạo và nâng cấp schema của Temporal trên PostgreSQL (chạy trong image admin-tools, cùng
# phiên bản với server). Database `temporal` và `temporal_visibility` do db-init tạo.
# Kết nối lấy từ SQL_PLUGIN / SQL_HOST / SQL_PORT / SQL_USER / SQL_PASSWORD.
# Chạy lại được ở mỗi lần triển khai: update-schema chỉ áp phần còn thiếu, nên nâng phiên
# bản Temporal = đổi HQ_TEMPORAL_VERSION rồi triển khai lại.
set -eu

SCHEMA=/etc/temporal/schema/postgresql/v12

for pair in temporal:temporal temporal_visibility:visibility; do
  db=${pair%%:*}
  dir=${pair##*:}
  echo "temporal schema: $db"
  temporal-sql-tool --db "$db" setup-schema -v 0.0
  temporal-sql-tool --db "$db" update-schema -d "$SCHEMA/$dir/versioned"
done
