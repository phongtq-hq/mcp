-- name: InsertLLMUsage :one
-- Một dòng usage cho một lời gọi LLM, kèm chi phí ước tính theo giá hiệu lực lúc
-- ghi (ưu tiên giá riêng của org, sau đó giá niêm yết). Không có giá thì
-- estimated_cost là NULL: usage vẫn được ghi, chi phí tính lại được sau.
--
-- input_tokens không gồm phần đọc từ cache và phần ghi cache (chúng có cột riêng).
-- reasoning_tokens đã nằm trong output_tokens với provider không tính riêng, nên chỉ
-- được cộng thêm khi bảng giá có reasoning_per_mtok.
WITH price AS (
  SELECT p.id, p.currency, p.input_per_mtok, p.output_per_mtok, p.cached_input_per_mtok, p.cache_write_per_mtok, p.reasoning_per_mtok
  FROM model_pricing p
  WHERE p.model_id = @model_id AND (p.org_id = @org_id OR p.org_id IS NULL)
    AND p.effective_from <= now() AND (p.effective_to IS NULL OR p.effective_to > now())
  ORDER BY (p.org_id IS NULL) ASC LIMIT 1
), u AS (
  SELECT CAST(@input_tokens AS bigint) AS input_tokens, CAST(@output_tokens AS bigint) AS output_tokens,
         CAST(@cached_input_tokens AS bigint) AS cached_input_tokens, CAST(@cache_write_tokens AS bigint) AS cache_write_tokens,
         CAST(@reasoning_tokens AS bigint) AS reasoning_tokens
)
INSERT INTO usage_records (id, org_id, project_id, user_id, agent_id, agent_version_id, execution_id, task_id, step_seq,
  kind, purpose, provider_id, model_id, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, reasoning_tokens,
  pricing_id, estimated_cost, currency)
SELECT @id, @org_id, @project_id, sqlc.narg('user_id'), @agent_id, @agent_version_id, @execution_id, @task_id, @step_seq,
  'llm', @purpose, m.provider_id, m.id, u.input_tokens, u.output_tokens, u.cached_input_tokens, u.cache_write_tokens, u.reasoning_tokens,
  price.id,
  CASE WHEN price.id IS NULL THEN NULL ELSE round((
      u.input_tokens        * price.input_per_mtok
    + u.output_tokens       * price.output_per_mtok
    + u.cached_input_tokens * COALESCE(price.cached_input_per_mtok, price.input_per_mtok)
    + u.cache_write_tokens  * COALESCE(price.cache_write_per_mtok, price.input_per_mtok)
    + u.reasoning_tokens    * COALESCE(price.reasoning_per_mtok, 0)
  ) / 1000000, 8) END,
  COALESCE(price.currency, 'USD')
FROM models m CROSS JOIN u LEFT JOIN price ON true
WHERE m.id = @model_id
RETURNING COALESCE(estimated_cost::text, '')::text AS estimated_cost, currency;

-- name: ListUsage :many
-- Bản ghi usage thô, mới nhất trước; khoảng thời gian bắt buộc (cắt phân vùng).
SELECT id, project_id, user_id, agent_id, execution_id, task_id, workflow_id, kind, purpose, provider_id, model_id, tool_id,
       input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, reasoning_tokens, pricing_id,
       COALESCE(estimated_cost::text, '')::text AS estimated_cost, currency, occurred_at
FROM usage_records
WHERE org_id = @org_id AND occurred_at >= @from_time AND occurred_at < @to_time
  AND (sqlc.narg('execution_id')::uuid IS NULL OR execution_id = sqlc.narg('execution_id'))
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id'))
  AND (sqlc.narg('model_id')::uuid IS NULL OR model_id = sqlc.narg('model_id'))
  AND (sqlc.narg('after_time')::timestamptz IS NULL
       OR (occurred_at, id) < (sqlc.narg('after_time')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY occurred_at DESC, id DESC LIMIT @row_limit;

-- name: SummarizeUsage :many
-- Tổng hợp usage theo tối đa hai chiều (báo cáo của Cost). Tên chiều là một giá trị trong
-- danh sách cố định, chọn cột bằng CASE: không có SQL nào được ghép từ input. Chiều rỗng
-- hoặc giá trị NULL cho khoá '' (với chiều rỗng: một nhóm duy nhất). Khoảng thời gian bắt buộc (cắt phân vùng). Chỉ
-- cộng tiền USD: mọi model chạy được đều phải có giá USD.
SELECT
  COALESCE((CASE @dim1::text WHEN 'project' THEN project_id::text WHEN 'user' THEN user_id::text WHEN 'agent' THEN agent_id::text
     WHEN 'model' THEN model_id::text WHEN 'provider' THEN provider_id::text WHEN 'workflow' THEN workflow_id::text
     WHEN 'tool' THEN tool_id::text WHEN 'purpose' THEN purpose END), '')::text AS key1,
  COALESCE((CASE @dim2::text WHEN 'project' THEN project_id::text WHEN 'user' THEN user_id::text WHEN 'agent' THEN agent_id::text
     WHEN 'model' THEN model_id::text WHEN 'provider' THEN provider_id::text WHEN 'workflow' THEN workflow_id::text
     WHEN 'tool' THEN tool_id::text WHEN 'purpose' THEN purpose END), '')::text AS key2,
  count(*)::bigint AS calls,
  count(DISTINCT execution_id)::bigint AS executions,
  COALESCE(sum(input_tokens), 0)::bigint AS input_tokens,
  COALESCE(sum(output_tokens), 0)::bigint AS output_tokens,
  COALESCE(sum(cached_input_tokens), 0)::bigint AS cached_input_tokens,
  COALESCE(sum(estimated_cost) FILTER (WHERE currency = 'USD'), 0)::numeric(18,8)::text AS estimated_cost,
  -- Trung bình trên các execution có chi phí: dòng không gắn execution và lượt chưa có
  -- giá không làm lệch tử số hay mẫu số.
  COALESCE(round(sum(estimated_cost) FILTER (WHERE currency = 'USD' AND execution_id IS NOT NULL)
    / NULLIF(count(DISTINCT execution_id) FILTER (WHERE currency = 'USD' AND estimated_cost IS NOT NULL), 0), 8)::text, '')::text AS avg_cost_per_execution,
  -- Lượt không góp vào chi phí: chưa có giá, hoặc có giá không phải USD.
  (count(*) FILTER (WHERE estimated_cost IS NULL OR currency <> 'USD'))::bigint AS unpriced_calls
FROM usage_records
WHERE org_id = @org_id AND occurred_at >= @from_time AND occurred_at < @to_time
  AND (sqlc.narg('project_id')::uuid IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id'))
  AND (sqlc.narg('model_id')::uuid IS NULL OR model_id = sqlc.narg('model_id'))
  AND (sqlc.narg('user_id')::uuid IS NULL OR user_id = sqlc.narg('user_id'))
GROUP BY 1, 2
ORDER BY sum(estimated_cost) FILTER (WHERE currency = 'USD') DESC NULLS LAST, 1, 2 LIMIT @row_limit;

-- name: UsageTimeseries :many
-- Chuỗi thời gian theo ngày hoặc tháng (UTC), tuỳ chọn tách theo một chiều.
SELECT
  date_trunc(@bucket::text, occurred_at, 'UTC')::timestamptz AS period_start,
  COALESCE((CASE @dim::text WHEN 'project' THEN project_id::text WHEN 'agent' THEN agent_id::text
     WHEN 'model' THEN model_id::text WHEN 'provider' THEN provider_id::text END), '')::text AS key,
  COALESCE(sum(input_tokens), 0)::bigint AS input_tokens,
  COALESCE(sum(output_tokens), 0)::bigint AS output_tokens,
  COALESCE(sum(estimated_cost) FILTER (WHERE currency = 'USD'), 0)::numeric(18,8)::text AS estimated_cost
FROM usage_records
WHERE org_id = @org_id AND occurred_at >= @from_time AND occurred_at < @to_time
  AND (sqlc.narg('project_id')::uuid IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id'))
GROUP BY 1, 2
ORDER BY 2, 1 LIMIT @row_limit;

-- name: ForecastMonth :one
-- Dự báo chi phí tới hết tháng (UTC) bằng ngoại suy tuyến tính, tính bằng NUMERIC trong
-- database (không qua float): chi phí từ đầu tháng tới giờ + chi phí trung bình / thấp
-- nhất / cao nhất của một ngày ĐÃ TRỌN trong tháng × số ngày còn lại. full_days = 0 thì
-- chưa có căn cứ để dự báo.
WITH days AS (
  SELECT date_trunc('day', occurred_at, 'UTC') AS d, sum(estimated_cost) AS cost
  FROM usage_records
  WHERE org_id = @org_id AND occurred_at >= sqlc.arg('month_start')::timestamptz AND occurred_at < sqlc.arg('now_time')::timestamptz AND currency = 'USD'
    AND (sqlc.narg('project_id')::uuid IS NULL OR project_id = sqlc.narg('project_id'))
    AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id'))
  GROUP BY 1
), full_days AS (
  SELECT COALESCE(days.cost, 0) AS cost
  FROM generate_series(sqlc.arg('month_start')::timestamptz, sqlc.arg('last_full_day')::timestamptz, make_interval(hours => 24)) AS g(d) -- 24 giờ tuyệt đối: không phụ thuộc múi giờ của phiên
  LEFT JOIN days ON days.d = g.d
), stats AS (
  SELECT count(*) AS n, COALESCE(avg(cost), 0) AS mean, COALESCE(min(cost), 0) AS low, COALESCE(max(cost), 0) AS high FROM full_days
), rest AS (
  SELECT (extract(epoch FROM (sqlc.arg('month_end')::timestamptz - sqlc.arg('now_time')::timestamptz)) / 86400)::numeric AS days_left,
         COALESCE((SELECT sum(cost) FROM days), 0) AS spent
)
SELECT stats.n::bigint AS full_days,
       round(rest.spent + stats.mean * rest.days_left, 8)::text AS projected,
       round(rest.spent + stats.low * rest.days_left, 8)::text AS low,
       round(rest.spent + stats.high * rest.days_left, 8)::text AS high
FROM stats, rest;
