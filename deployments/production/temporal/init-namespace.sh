#!/bin/sh
# Namespace và search attribute mà platform cần (chạy trong image admin-tools, qua mTLS;
# kết nối lấy từ các biến TEMPORAL_*). Chạy lại được.
set -eu

ns=${TEMPORAL_NAMESPACE:-default}

# Frontend mở cổng trước khi sẵn sàng trả lời: chờ có trần.
i=0
until temporal operator cluster health >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "temporal-init: frontend không trả lời sau 120 giây" >&2
    temporal operator cluster health || true
    exit 1
  fi
  sleep 2
done

if temporal operator namespace describe -n "$ns" >/dev/null 2>&1; then
  echo "temporal-init: namespace $ns đã có"
else
  temporal operator namespace create -n "$ns" --retention "${HQ_TEMPORAL_RETENTION:-168h}"
fi

# Platform gắn OrgId / ProjectId vào mọi workflow; thiếu chúng thì không tạo được execution.
# Namespace vừa tạo cần vài giây mới vào cache của frontend: thử lại có trần.
i=0
until existing=$(temporal operator search-attribute list -n "$ns" 2>/dev/null); do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then
    echo "temporal-init: không đọc được search attribute của namespace $ns sau 60 giây" >&2
    temporal operator search-attribute list -n "$ns" || true
    exit 1
  fi
  sleep 2
done
for attr in OrgId ProjectId; do
  if printf '%s\n' "$existing" | grep -qw "$attr"; then
    echo "temporal-init: search attribute $attr đã có"
  else
    temporal operator search-attribute create -n "$ns" --name "$attr" --type Keyword
  fi
done
