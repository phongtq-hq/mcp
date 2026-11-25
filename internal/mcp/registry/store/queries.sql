-- name: InsertServer :one
INSERT INTO mcp_servers (id, org_id, slug, name, description, kind, transport, endpoint, auth_method)
VALUES (@id, sqlc.narg('org_id'), @slug, @name, sqlc.narg('description'), @kind, 'streamable_http', @endpoint, @auth_method)
RETURNING id;

-- name: SlugUsedInOtherScope :one
-- Slug của platform và slug của org dùng chung một không gian tên: tên tool phía
-- client và đích của ngữ cảnh ký đều là slug.
SELECT EXISTS (
  SELECT 1 FROM mcp_servers
  WHERE slug = @slug AND status <> 'archived' AND ((@platform::boolean AND org_id IS NOT NULL) OR (NOT @platform::boolean AND org_id IS NULL))
);

-- name: LockServerBySlug :one
SELECT id, org_id, slug, kind, endpoint, status, active_version_id FROM mcp_servers
WHERE slug = @slug AND org_id IS NOT DISTINCT FROM sqlc.narg('org_id') AND status <> 'archived' FOR UPDATE;

-- name: GetVersionHash :one
SELECT id, version, snapshot_hash, review_status FROM mcp_server_versions WHERE server_id = @server_id AND id = @id;

-- name: GetVersionByNumber :one
SELECT id, version, snapshot_hash, review_status FROM mcp_server_versions WHERE server_id = @server_id AND version = @version;

-- name: LatestVersion :one
SELECT id, version, snapshot_hash, review_status FROM mcp_server_versions
WHERE server_id = @server_id ORDER BY version DESC LIMIT 1;

-- name: InsertVersion :one
INSERT INTO mcp_server_versions (id, org_id, server_id, version, reported_version, protocol_version, snapshot, snapshot_hash, review_status, reviewed_at)
VALUES (@id, sqlc.narg('org_id'), @server_id, @version, sqlc.narg('reported_version'), @protocol_version, @snapshot, @snapshot_hash, @review_status,
  CASE WHEN @review_status::text = 'approved' THEN now() END)
RETURNING id;

-- name: ReviewVersion :execrows
UPDATE mcp_server_versions SET review_status = @review_status, reviewed_by = sqlc.narg('reviewed_by'), reviewed_at = now()
WHERE server_id = @server_id AND id = @id AND review_status = 'pending_review';

-- name: ActivateVersion :execrows
-- Server đã bị tắt hoặc lưu trữ không tự bật lại khi một version được duyệt.
UPDATE mcp_servers SET active_version_id = @active_version_id, status = 'active', updated_at = clock_timestamp()
WHERE id = @id AND status IN ('registered','active');

-- name: InheritPolicy :exec
-- Trước khi kích hoạt một version: tool có định nghĩa không đổi lấy thuộc tính chính
-- sách HIỆN TẠI của version đang hoạt động, không phải bản chép lúc discovery (người
-- vận hành có thể đã tắt tool trong lúc version mới chờ duyệt).
UPDATE mcp_tools n SET enabled = o.enabled, risk_level = o.risk_level, side_effect = o.side_effect,
  data_sensitivity = o.data_sensitivity, egress = o.egress, cacheable = o.cacheable,
  timeout_ms = o.timeout_ms, max_result_bytes = o.max_result_bytes, updated_at = clock_timestamp()
FROM mcp_tools o
WHERE n.server_version_id = @new_version_id AND o.server_version_id = @active_version_id
  AND o.name = n.name AND o.definition_hash = n.definition_hash;

-- name: TightenChangedPolicy :exec
-- Cùng thời điểm: tool có định nghĩa ĐÃ ĐỔI chỉ được giữ hoặc siết theo phân loại HIỆN TẠI
-- của version đang hoạt động (người vận hành có thể đã tắt tool hay gắn nhãn sensitive
-- trong lúc version mới chờ duyệt; bản chép lúc discovery không thấy thay đổi đó). Tool đang
-- tắt chỉ vì bị khoá (locked_at) không tính là bị tắt.
UPDATE mcp_tools n SET
  enabled = n.enabled AND (o.enabled OR o.locked_at IS NOT NULL),
  risk_level = CASE WHEN array_position(ARRAY['read','write','destructive','financial'], o.risk_level)
                       > array_position(ARRAY['read','write','destructive','financial'], n.risk_level)
                    THEN o.risk_level ELSE n.risk_level END,
  data_sensitivity = CASE WHEN array_position(ARRAY['public','internal','sensitive'], o.data_sensitivity)
                             > array_position(ARRAY['public','internal','sensitive'], n.data_sensitivity)
                          THEN o.data_sensitivity ELSE n.data_sensitivity END,
  egress = n.egress OR o.egress,
  cacheable = n.cacheable AND o.cacheable,
  timeout_ms = LEAST(n.timeout_ms, o.timeout_ms),
  max_result_bytes = LEAST(n.max_result_bytes, o.max_result_bytes),
  updated_at = clock_timestamp()
FROM mcp_tools o
WHERE n.server_version_id = @new_version_id AND o.server_version_id = @active_version_id
  AND o.name = n.name AND o.definition_hash <> n.definition_hash;

-- name: SetServerStatus :execrows
UPDATE mcp_servers SET status = @status, updated_at = clock_timestamp() WHERE id = @id;

-- name: ListToolsOfVersion :many
SELECT id, name, definition_hash, enabled, risk_level, side_effect, data_sensitivity, egress, cacheable, timeout_ms, max_result_bytes, locked_at
FROM mcp_tools WHERE server_version_id = @server_version_id;

-- name: InsertTool :exec
INSERT INTO mcp_tools (id, org_id, server_id, server_version_id, name, description, input_schema, definition_hash,
  enabled, risk_level, side_effect, data_sensitivity, egress, cacheable, timeout_ms, max_result_bytes)
VALUES (@id, sqlc.narg('org_id'), @server_id, @server_version_id, @name, @description, @input_schema, @definition_hash,
  @enabled, @risk_level, @side_effect, @data_sensitivity, @egress, @cacheable, @timeout_ms, @max_result_bytes);

-- name: DisableToolsByID :exec
-- Tạm khoá (định nghĩa đổi): khác với người vận hành tắt tool, xem migration 00039.
UPDATE mcp_tools SET enabled = false, locked_at = clock_timestamp(), updated_at = clock_timestamp() WHERE id = ANY(@ids::uuid[]);

-- name: ClassifyTool :execrows
UPDATE mcp_tools SET
  enabled          = COALESCE(sqlc.narg('enabled'), enabled),
  risk_level       = COALESCE(sqlc.narg('risk_level'), risk_level),
  side_effect      = COALESCE(sqlc.narg('side_effect'), side_effect),
  data_sensitivity = COALESCE(sqlc.narg('data_sensitivity'), data_sensitivity),
  egress           = COALESCE(sqlc.narg('egress'), egress),
  -- Người vận hành bật hay tắt tường minh: tool không còn là "bị khoá".
  locked_at        = CASE WHEN sqlc.narg('enabled')::bool IS NULL THEN locked_at END,
  updated_at       = clock_timestamp()
WHERE server_version_id = @server_version_id AND name = @name;

-- name: ListServers :many
SELECT s.id, s.org_id, s.slug, s.name, s.description, s.kind, s.transport, s.endpoint, s.auth_method, s.status,
       s.health_status, s.health_checked_at, s.created_at, s.updated_at, av.version AS active_version,
       COALESCE((SELECT max(p.version) FROM mcp_server_versions p WHERE p.server_id = s.id AND p.review_status = 'pending_review'), 0)::int AS pending_version
FROM mcp_servers s
LEFT JOIN mcp_server_versions av ON av.server_id = s.id AND av.id = s.active_version_id
WHERE (s.org_id = @org_id OR s.org_id IS NULL) AND s.status <> 'archived'
  AND (sqlc.narg('id')::uuid IS NULL OR s.id = sqlc.narg('id'))
  AND (sqlc.narg('after_created')::timestamptz IS NULL
       OR (s.created_at, s.id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY s.created_at DESC, s.id DESC LIMIT @row_limit;

-- name: ListActiveTools :many
-- Tool của version đang hoạt động của các server đang hoạt động mà tenant thấy được.
SELECT t.id, t.server_id, s.slug AS server_slug, s.kind AS server_kind, s.endpoint AS server_endpoint, s.auth_method AS server_auth_method,
       s.secret_id AS server_secret_id,
       t.name, t.description, t.input_schema, t.definition_hash, t.enabled, t.risk_level, t.side_effect,
       t.data_sensitivity, t.egress, t.cacheable, t.timeout_ms, t.max_result_bytes
FROM mcp_tools t
JOIN mcp_servers s ON s.id = t.server_id AND s.active_version_id = t.server_version_id
JOIN mcp_server_versions v ON v.id = s.active_version_id AND v.review_status = 'approved'
WHERE (s.org_id = @org_id OR s.org_id IS NULL) AND s.status = 'active'
  AND (sqlc.narg('server_id')::uuid IS NULL OR s.id = sqlc.narg('server_id'))
  AND (sqlc.narg('enabled')::bool IS NULL OR t.enabled = sqlc.narg('enabled'))
  AND (sqlc.narg('risk_level')::text IS NULL OR t.risk_level = sqlc.narg('risk_level'))
  AND (sqlc.narg('tool_id')::uuid IS NULL OR t.id = sqlc.narg('tool_id'))
  -- Chính sách project (migration 00041): server giới hạn theo project chỉ có mặt ở project
  -- đã được gắn. project_id NULL: không lọc (danh sách quản trị cấp org).
  AND (sqlc.narg('project_id')::uuid IS NULL OR NOT s.project_scoped OR EXISTS (
        SELECT 1 FROM mcp_server_projects b
        WHERE b.org_id = s.org_id AND b.server_id = s.id AND b.project_id = sqlc.narg('project_id')))
ORDER BY s.slug, (s.org_id IS NOT NULL), t.name LIMIT @row_limit;

-- name: GetOrgServer :one
-- Server của chính org (không gồm server của platform), kèm cấu hình xác thực.
SELECT id, org_id, slug, name, kind, transport, endpoint, auth_method, secret_id,
       status, active_version_id, updated_at
FROM mcp_servers WHERE org_id = @org_id AND id = @id AND status <> 'archived';

-- name: SetServerAuth :exec
UPDATE mcp_servers SET secret_id = @secret_id, updated_at = clock_timestamp() WHERE org_id = @org_id AND id = @id;

-- name: UpdateServerMeta :execrows
UPDATE mcp_servers SET
  name        = COALESCE(sqlc.narg('name'), name),
  description = COALESCE(sqlc.narg('description'), description),
  status      = COALESCE(sqlc.narg('status'), status),
  updated_at  = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND status <> 'archived'
  AND (sqlc.narg('if_updated_at')::timestamptz IS NULL OR updated_at = sqlc.narg('if_updated_at'));

-- name: ArchiveServer :execrows
UPDATE mcp_servers SET status = 'archived', updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND status <> 'archived';

-- name: ListVersions :many
SELECT v.id, v.server_id, v.version, v.reported_version, v.protocol_version, v.review_status, v.reviewed_by, v.reviewed_at,
       v.discovered_at, (SELECT count(*) FROM mcp_tools t WHERE t.server_version_id = v.id)::int AS tool_count
FROM mcp_server_versions v
WHERE v.server_id = @server_id
  AND (sqlc.narg('review_status')::text IS NULL OR v.review_status = sqlc.narg('review_status'))
  AND v.version < @before_version
ORDER BY v.version DESC LIMIT @row_limit;
