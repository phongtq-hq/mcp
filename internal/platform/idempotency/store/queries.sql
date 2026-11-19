-- name: DeleteStaleKeys :exec
-- Dọn key in_progress bị bỏ dở của đúng request này, và (cơ hội) một lô key đã
-- hết hạn của org. Thời gian so bằng đồng hồ database.
DELETE FROM idempotency_keys k
WHERE k.org_id = @org_id AND (
  (k.principal_id = @principal_id AND k.key = @key
     AND (k.expires_at < now() OR (k.status = 'in_progress' AND k.created_at < now() - make_interval(secs => @abandoned_after_secs::int))))
  OR (k.principal_id, k.key) IN (
     SELECT e.principal_id, e.key FROM idempotency_keys e
     WHERE e.org_id = @org_id AND e.expires_at < now() LIMIT 100));

-- name: ClaimKey :one
-- Trả về thời điểm nhận key (định danh của lần nhận này); không có dòng nghĩa là key đã có chủ.
INSERT INTO idempotency_keys (org_id, principal_id, key, request_hash, status, expires_at)
VALUES (@org_id, @principal_id, @key, @request_hash, 'in_progress', now() + make_interval(secs => @ttl_secs::int))
ON CONFLICT (org_id, principal_id, key) DO NOTHING
RETURNING created_at;

-- name: GetKey :one
SELECT request_hash, status, response_code, response_headers, response_body FROM idempotency_keys
WHERE org_id = @org_id AND principal_id = @principal_id AND key = @key;

-- name: CompleteKey :exec
-- Khớp theo created_at: chỉ lần nhận này mới hoàn tất được key của nó.
UPDATE idempotency_keys SET status = 'completed', response_code = @response_code,
  response_headers = @response_headers, response_body = @response_body
WHERE org_id = @org_id AND principal_id = @principal_id AND key = @key
  AND created_at = @claimed_at AND status = 'in_progress';

-- name: ReleaseKey :exec
DELETE FROM idempotency_keys
WHERE org_id = @org_id AND principal_id = @principal_id AND key = @key
  AND created_at = @claimed_at AND status = 'in_progress';
