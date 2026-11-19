-- name: InsertExecution :one
INSERT INTO agent_executions (id, org_id, project_id, initiator_type, initiator_id, mode, input, trace_id, deadline_at, budget_limit, plan, plan_source,
                              workflow_execution_id)
VALUES (@id, @org_id, @project_id, @initiator_type, @initiator_id, @mode, @input, sqlc.narg('trace_id'), @deadline_at,
        CAST(sqlc.narg('budget_limit') AS text)::numeric, sqlc.narg('plan'), sqlc.narg('plan_source'), sqlc.narg('workflow_execution_id'))
RETURNING *;

-- name: InsertTask :exec
INSERT INTO agent_tasks (id, org_id, execution_id, agent_id, agent_version_id, goal, system_prompt, model_chain, budget_limit,
                         plan_key, depends_on, on_failure)
VALUES (@id, @org_id, @execution_id, @agent_id, @agent_version_id, @goal, @system_prompt, @model_chain,
        CAST(sqlc.narg('budget_limit') AS text)::numeric,
        sqlc.narg('plan_key'), COALESCE(sqlc.narg('depends_on')::uuid[], '{}'), COALESCE(sqlc.narg('on_failure')::text, 'skip_dependents'));

-- name: GetTaskBudget :one
-- Budget của task (limits.budget của Agent Version, chụp lúc tạo) và mức đã tiêu.
-- Mức đã tiêu gồm cả phần các task con (delegation) đã tiêu: tính từ cây task.
SELECT COALESCE(t.budget_limit::text, '')::text AS budget_limit,
       agent_task_spent(t.org_id, t.id)::text AS estimated_cost
FROM agent_tasks t WHERE t.org_id = @org_id AND t.id = @id;

-- name: GetExecution :one
SELECT * FROM agent_executions WHERE org_id = @org_id AND id = @id;

-- name: GetExecutionIDForWorkflowRun :one
-- Execution đã được tạo cho một lần chạy workflow (nhiều nhất một, migration 00032).
SELECT id FROM agent_executions WHERE org_id = @org_id AND workflow_execution_id = @workflow_execution_id;

-- name: ListExecutions :many
SELECT * FROM agent_executions
WHERE org_id = @org_id AND project_id = @project_id
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: RequestCancel :execrows
UPDATE agent_executions SET cancel_requested_at = COALESCE(cancel_requested_at, now()), updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused');

-- name: StartExecution :execrows
UPDATE agent_executions SET status = 'running', started_at = now(), updated_at = now()
WHERE org_id = @org_id AND id = @id AND status = 'queued';

-- name: StartTask :exec
UPDATE agent_tasks SET status = 'running', started_at = now(), updated_at = now()
WHERE org_id = @org_id AND id = @id AND status = 'queued';

-- name: FinishExecution :execrows
UPDATE agent_executions SET status = @status, output = sqlc.narg('output'), error = sqlc.narg('error'),
  finished_at = now(), updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused');

-- name: FinishTasks :exec
-- Đóng mọi task còn mở của execution cùng trạng thái với nó.
UPDATE agent_tasks SET status = @status, output = sqlc.narg('output'), error = sqlc.narg('error'),
  finished_at = now(), updated_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND status IN ('queued','running','waiting_approval','paused');

-- name: ExpireExecution :execrows
-- Execution mồ côi: process chạy nó đã chết (Phase 1 không bền vững qua restart).
-- Khoảng trễ một phút để không tranh với vòng lặp đang tự đóng execution.
UPDATE agent_executions SET status = 'timed_out', finished_at = now(), updated_at = now(),
  error = '{"code":"timed_out","message":"execution vượt thời hạn hoặc process chạy nó đã dừng"}'::jsonb
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused') AND deadline_at < now() - interval '1 minute';

-- name: CancelRequested :one
SELECT (cancel_requested_at IS NOT NULL)::bool AS requested FROM agent_executions WHERE org_id = @org_id AND id = @id;

-- name: LockTask :one
-- NO KEY UPDATE: đủ để tuần tự hoá việc ghi step, không chặn khoá ngoại của task con.
SELECT id, status, execution_id, agent_id, agent_version_id, model_chain, created_at FROM agent_tasks
WHERE org_id = @org_id AND id = @id FOR NO KEY UPDATE;

-- name: LockExecution :one
-- Thứ tự khoá ở mọi đường ghi: execution → task → step.
SELECT id, status, project_id, initiator_type, initiator_id, created_at, cancel_requested_at FROM agent_executions
WHERE org_id = @org_id AND id = @id FOR NO KEY UPDATE;

-- name: LockOrgAdmission :exec
-- Tuần tự hoá việc nhận execution mới của một org, để trần đồng thời đếm đúng.
SELECT pg_advisory_xact_lock(hashtext('exec_admit'), hashtext(CAST(@org_id AS uuid)::text));

-- name: GetStep :one
SELECT id, kind, status, response, error, created_at FROM execution_steps
WHERE org_id = @org_id AND task_id = @task_id AND step_seq = @step_seq
  AND created_at >= @since   -- cắt phân vùng: step không cũ hơn task của nó
ORDER BY created_at DESC LIMIT 1;

-- name: InsertStep :exec
INSERT INTO execution_steps (id, org_id, execution_id, task_id, step_seq, kind, status, model_id, tool_name, args_hash, request, error)
VALUES (@id, @org_id, @execution_id, @task_id, @step_seq, @kind, @status, sqlc.narg('model_id'), sqlc.narg('tool_name'),
        sqlc.narg('args_hash'), sqlc.narg('request'), sqlc.narg('error'));

-- name: FinishStep :execrows
UPDATE execution_steps SET status = @status, response = sqlc.narg('response'), error = sqlc.narg('error'),
  duration_ms = @duration_ms, routing = sqlc.narg('routing'), model_id = COALESCE(sqlc.narg('model_id'), model_id)
WHERE org_id = @org_id AND task_id = @task_id AND step_seq = @step_seq AND status = 'started'
  AND created_at >= @since;

-- name: AddTaskProgress :exec
UPDATE agent_tasks SET iterations = iterations + @iterations, tool_calls = tool_calls + @tool_calls,
  estimated_cost = estimated_cost + COALESCE(NULLIF(@cost::text, '')::numeric, 0), updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused');

-- name: AddExecutionCost :exec
-- Số cộng dồn chỉ để hiển thị; nguồn sự thật là usage_records. Execution đã bị đóng
-- (hết hạn) thì bỏ qua, để usage ghi trễ không bị trigger trạng thái kết thúc chặn.
UPDATE agent_executions SET estimated_cost = estimated_cost + COALESCE(NULLIF(@cost::text, '')::numeric, 0), updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused');

-- name: ListTasks :many
-- Chỉ các cột API trả ra: system_prompt và model_chain của task không được đọc ở đây.
SELECT t.id, t.execution_id, t.parent_task_id, t.agent_id, t.depth, t.goal, t.status, t.output, t.error,
       t.estimated_cost, t.iterations, t.tool_calls, t.started_at, t.finished_at, t.created_at,
       t.plan_key, t.depends_on, t.on_failure, t.output_summary, COALESCE(t.budget_limit::text, '')::text AS budget_limit,
       v.version AS agent_version
FROM agent_tasks t
JOIN agent_versions v ON v.id = t.agent_version_id AND v.org_id = t.org_id
WHERE t.org_id = @org_id AND t.execution_id = @execution_id
ORDER BY t.created_at, t.id LIMIT @row_limit;

-- name: GetTask :one
SELECT t.id, t.execution_id, t.parent_task_id, t.agent_id, t.depth, t.goal, t.status, t.output, t.error,
       t.estimated_cost, t.iterations, t.tool_calls, t.started_at, t.finished_at, t.created_at,
       t.plan_key, t.depends_on, t.on_failure, t.output_summary, COALESCE(t.budget_limit::text, '')::text AS budget_limit,
       v.version AS agent_version, e.project_id
FROM agent_tasks t
JOIN agent_versions v ON v.id = t.agent_version_id AND v.org_id = t.org_id
JOIN agent_executions e ON e.id = t.execution_id AND e.org_id = t.org_id
WHERE t.org_id = @org_id AND t.id = @id;

-- name: ListSteps :many
SELECT id, task_id, step_seq, kind, status, model_id, routing, tool_id, tool_name, approval_id, duration_ms,
  (CASE WHEN @include_content::bool THEN request END)::jsonb AS request,
  (CASE WHEN @include_content::bool THEN response END)::jsonb AS response,
  error, created_at
FROM execution_steps
WHERE org_id = @org_id AND task_id = @task_id AND step_seq > @after_seq AND created_at >= @since
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind'))
ORDER BY step_seq LIMIT @row_limit;

-- name: GetActiveTask :one
-- Cho Gateway: task của một execution token còn chạy và chưa bị yêu cầu huỷ.
SELECT t.id, t.execution_id, t.agent_id, t.agent_version_id, t.iterations, t.model_chain,
       COALESCE(t.budget_limit::text, '')::text AS budget_limit, agent_task_spent(t.org_id, t.id)::text AS estimated_cost, t.depth,
       (e.plan_source IS NOT DISTINCT FROM 'model')::bool AS model_planned,
       e.project_id, e.initiator_type, e.initiator_id, e.deadline_at
FROM agent_tasks t JOIN agent_executions e ON e.id = t.execution_id AND e.org_id = t.org_id
-- Execution có nhiều task (planned) ở waiting_approval khi MỘT task của nó chờ duyệt;
-- các task khác vẫn chạy. Task đang chờ thì tự nó không ở running.
-- Tương tự với paused: execution sang paused khi MỘT task của nó dừng ở ranh giới bước,
-- còn task khác đang giữa bước phải được chạy xong bước đó.
WHERE t.org_id = @org_id AND t.id = @id AND t.status = 'running' AND e.status IN ('running','waiting_approval','paused')
  AND e.cancel_requested_at IS NULL AND e.deadline_at > now()
  -- Task đứng sau một cổng approval chưa mở không được phục vụ, dù nơi chạy kế hoạch đã
  -- đặt nó sang running: cổng do database giữ (migration 00033).
  AND agent_task_gates_open(t.id);

-- name: GetTaskContext :one
-- Nội dung mở đầu hội thoại của một task: yêu cầu của người dùng và system prompt đã
-- chụp lúc tạo task. Các bước đọc lại từ đây thay vì mang nội dung theo.
SELECT t.goal, COALESCE(t.system_prompt, '')::text AS system_prompt, t.execution_id, t.depends_on, t.parent_task_id,
       (e.plan_source IS NOT DISTINCT FROM 'model')::bool AS model_planned
FROM agent_tasks t JOIN agent_executions e ON e.id = t.execution_id AND e.org_id = t.org_id
WHERE t.org_id = @org_id AND t.id = @id;

-- name: ListDependencyResults :many
-- Kết quả của các task mà một task phụ thuộc (cùng execution): chỉ tóm tắt, không phải
-- toàn bộ hội thoại của chúng.
SELECT id, COALESCE(plan_key, '')::text AS plan_key, status, COALESCE(output_summary, '')::text AS output_summary, on_failure
FROM agent_tasks
WHERE org_id = @org_id AND execution_id = @execution_id AND id = ANY(@ids::uuid[])
ORDER BY created_at, id LIMIT 32;

-- name: ListContextSteps :many
-- Các step trước một step, theo thứ tự, để dựng lại hội thoại.
SELECT step_seq, kind, status, request, response, error FROM execution_steps
WHERE org_id = @org_id AND task_id = @task_id AND step_seq < @before_seq AND created_at >= @since
ORDER BY step_seq, created_at, id LIMIT @row_limit;

-- name: CountActiveSlots :one
-- Số vòng lặp Agent có thể chạy đồng thời của org (trần đồng thời theo org): execution
-- đơn Agent chiếm một suất, execution planned chiếm số task của Agent nó được chạy song
-- song (nút chờ không chạy vòng lặp nào nên không chiếm suất). Execution đã quá hạn
-- không chiếm suất.
SELECT COALESCE(sum(CASE WHEN mode = 'planned' AND jsonb_typeof(plan->'tasks') = 'array'
    THEN GREATEST(1, LEAST(COALESCE((plan->>'max_parallel_tasks')::int, 1),
      (SELECT count(*) FROM jsonb_array_elements(plan->'tasks') AS t(node) WHERE COALESCE(t.node->>'kind', '') NOT IN ('wait','approval'))::int))
    ELSE 1 END), 0)::bigint AS slots
FROM agent_executions
WHERE org_id = @org_id AND status IN ('queued','running','waiting_approval','paused') AND deadline_at > now();

-- name: CloseOpenSteps :exec
-- Khi task kết thúc: step còn started (bước bị ngắt, không ai chạy lại) thành failed.
UPDATE execution_steps SET status = 'failed', error = @error
WHERE org_id = @org_id AND task_id = @task_id AND status = 'started' AND created_at >= @since;

-- name: CloseOpenStepsOfExecution :exec
-- Khi execution bị đóng vì quá hạn mà không còn ai chạy nó.
UPDATE execution_steps SET status = 'failed', error = @error
WHERE org_id = @org_id AND execution_id = @execution_id AND status = 'started' AND created_at >= @since;

-- name: SetStepApproval :exec
-- Step của tool call đang chờ duyệt ghi lại approval của nó (một lần, khi còn started).
UPDATE execution_steps SET approval_id = @approval_id
WHERE org_id = @org_id AND task_id = @task_id AND step_seq = @step_seq AND status = 'started' AND approval_id IS NULL
  AND created_at >= @since;

-- name: SetTaskWaiting :exec
UPDATE agent_tasks SET status = @to_status, updated_at = now()
WHERE org_id = @org_id AND id = @id AND status = @from_status;

-- name: StartPlanTask :execrows
UPDATE agent_tasks SET status = 'running', started_at = now(), updated_at = now()
WHERE org_id = @org_id AND id = @id AND execution_id = @execution_id AND status = 'queued';

-- name: FinishTask :execrows
-- Đóng MỘT task (execution nhiều task). output_summary là phần các task phụ thuộc thấy.
UPDATE agent_tasks SET status = @status, output = sqlc.narg('output'), output_summary = sqlc.narg('output_summary'),
  error = sqlc.narg('error'), finished_at = now(), updated_at = now()
WHERE org_id = @org_id AND id = @id AND execution_id = @execution_id
  -- Task chưa từng bắt đầu không thể "hoàn thành": tham chiếu đi qua Temporal không
  -- được biến một task chưa chạy thành completed.
  AND (status IN ('running','waiting_approval','paused')
       OR (status = 'queued' AND CAST(@status AS text) IN ('cancelled','failed','timed_out')));

-- name: SkipTask :execrows
-- Task không được chạy vì một task nó phụ thuộc không hoàn thành.
UPDATE agent_tasks SET status = 'skipped', finished_at = now(), updated_at = now()
WHERE org_id = @org_id AND id = @id AND execution_id = @execution_id AND status = 'queued';

-- name: CountHeldTasks :one
-- Số task đang chờ duyệt và số task đang tạm dừng của execution.
SELECT count(*) FILTER (WHERE status = 'waiting_approval') AS waiting,
       count(*) FILTER (WHERE status = 'paused') AS paused
FROM agent_tasks WHERE org_id = @org_id AND execution_id = @execution_id;

-- name: SetExecutionHeld :exec
-- Execution chuyển giữa running / waiting_approval / paused theo trạng thái các task của nó.
UPDATE agent_executions SET status = @to_status, updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('running','waiting_approval','paused') AND status <> @to_status;

-- name: ExecutionControl :one
-- Yêu cầu từ bên ngoài mà vòng lặp đọc ở ranh giới bước: huỷ và tạm dừng.
SELECT (cancel_requested_at IS NOT NULL)::bool AS cancelled, (pause_requested_at IS NOT NULL)::bool AS paused
FROM agent_executions WHERE org_id = @org_id AND id = @id;

-- name: RequestPause :execrows
-- Chỉ tác động khi execution còn mở và CHƯA có yêu cầu tạm dừng: yêu cầu lặp lại không đổi gì.
UPDATE agent_executions SET pause_requested_at = now(), pause_requested_by_type = @by_type, pause_requested_by = @by_id, updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused') AND pause_requested_at IS NULL;

-- name: RequestResume :execrows
-- Chỉ tác động khi execution còn mở và ĐANG có yêu cầu tạm dừng.
UPDATE agent_executions SET pause_requested_at = NULL, pause_requested_by_type = NULL, pause_requested_by = NULL, updated_at = now()
WHERE org_id = @org_id AND id = @id AND status IN ('queued','running','waiting_approval','paused') AND pause_requested_at IS NOT NULL;

-- name: ListOpenTaskIDs :many
SELECT id FROM agent_tasks
WHERE org_id = @org_id AND execution_id = @execution_id AND status IN ('queued','running','waiting_approval','paused')
ORDER BY created_at, id LIMIT 64;

-- name: GetExecutionPlan :one
SELECT mode, plan, deadline_at, cancel_requested_at FROM agent_executions WHERE org_id = @org_id AND id = @id;

-- name: ListPlanTasks :many
-- Trạng thái và kết quả các task của một execution, để tổng hợp kết quả của kế hoạch.
SELECT id, status, output, COALESCE(output_summary, '')::text AS output_summary
FROM agent_tasks WHERE org_id = @org_id AND execution_id = @execution_id
ORDER BY created_at, id LIMIT 64;

-- name: InsertDelegationSource :exec
INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor)
VALUES (@org_id, @execution_id, @agent_version_id, @can_delegate_to, @max_depth, CAST(@budget_floor AS text)::numeric);

-- name: InsertDelegationTarget :exec
INSERT INTO execution_delegation_targets (org_id, execution_id, agent_id, agent_version_id, slug, model_chain, budget, budget_floor, spec, timeout_seconds)
VALUES (@org_id, @execution_id, @agent_id, @agent_version_id, @slug, @model_chain,
        CAST(sqlc.narg('budget') AS text)::numeric, CAST(@budget_floor AS text)::numeric, @spec, @timeout_seconds);

-- name: ListTaskChain :many
-- Một task và các tổ tiên của nó trong cây giao việc (task trước, gốc sau), kèm cấu hình
-- delegation của Agent Version từng task đang chạy. Có trần độ dài.
WITH RECURSIVE chain AS (
  SELECT t.id, t.agent_id, t.agent_version_id, t.parent_task_id, t.depth, t.execution_id, 0 AS hop
  FROM agent_tasks t WHERE t.org_id = @org_id AND t.id = @id
  UNION ALL
  SELECT p.id, p.agent_id, p.agent_version_id, p.parent_task_id, p.depth, p.execution_id, c.hop + 1
  FROM agent_tasks p JOIN chain c ON p.id = c.parent_task_id
  WHERE p.org_id = @org_id AND c.hop < 8
)
SELECT c.id, c.agent_id, c.agent_version_id, c.parent_task_id, c.depth, c.execution_id, c.hop,
       COALESCE(s.max_depth, 0)::int AS max_depth, COALESCE(s.can_delegate_to, '{}')::uuid[] AS can_delegate_to
FROM chain c
LEFT JOIN execution_delegation_sources s
  ON s.org_id = @org_id AND s.execution_id = c.execution_id AND s.agent_version_id = c.agent_version_id
ORDER BY c.hop;

-- name: ListDelegationTargets :many
SELECT agent_id, agent_version_id, slug, model_chain, spec, timeout_seconds
FROM execution_delegation_targets WHERE org_id = @org_id AND execution_id = @execution_id
ORDER BY slug LIMIT 32;

-- name: DelegateTask :one
-- Điểm quyết định của delegation (hàm trong database, migration 00023).
SELECT agent_task_delegate(@parent_task_id::uuid, @step_seq::int, @target_agent_id::uuid, @goal::text)::jsonb AS result;

-- name: GetDelegatedTask :one
-- Task con mà một bước của task cha đã giao việc.
SELECT id, agent_id, status, COALESCE(output_summary, '')::text AS output_summary FROM agent_tasks
WHERE org_id = @org_id AND parent_task_id = @parent_task_id AND delegated_step_seq = @step_seq;

-- name: CloseOpenDescendants :many
-- Khi một task kết thúc, mọi task con (và con của chúng) còn mở bị đóng theo: không ai
-- chạy chúng nữa, và chúng không được tiếp tục gọi Gateway.
WITH RECURSIVE sub AS (
  SELECT c.id, 1 AS hop FROM agent_tasks c WHERE c.org_id = @org_id AND c.parent_task_id = @id
  UNION ALL
  SELECT c.id, s.hop + 1 FROM agent_tasks c JOIN sub s ON c.parent_task_id = s.id WHERE c.org_id = @org_id AND s.hop < 8
)
UPDATE agent_tasks t SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE t.org_id = @org_id AND t.id IN (SELECT id FROM sub) AND t.status IN ('queued','running','waiting_approval','paused')
RETURNING t.id;

-- name: CloseOpenStepsOfTasks :exec
UPDATE execution_steps SET status = 'failed', error = @error
WHERE org_id = @org_id AND task_id = ANY(@task_ids::uuid[]) AND status = 'started' AND created_at >= @since;

-- name: FinishOtherTasks :exec
-- Đóng mọi task còn mở của execution trừ một task, KHÔNG kèm output: output của task
-- gốc không được ghi vào task của Agent khác.
UPDATE agent_tasks SET status = @status, error = sqlc.narg('error'), finished_at = now(), updated_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND id <> @except_id AND status IN ('queued','running','waiting_approval','paused');

-- name: InsertPlanProposal :exec
-- Đánh dấu một execution là lần lập kế hoạch, cùng transaction tạo nó.
INSERT INTO plan_proposals (org_id, execution_id, project_id, planner_agent_id, agent_ids, requested_by_type, requested_by)
VALUES (@org_id, @execution_id, @project_id, @planner_agent_id, @agent_ids, @requested_by_type, @requested_by);

-- name: GetPlanProposal :one
SELECT planner_agent_id, agent_ids, requested_by_type, requested_by, created_at FROM plan_proposals WHERE org_id = @org_id AND execution_id = @execution_id;

-- name: GetExecutionState :one
-- Trạng thái của execution cho luồng sự kiện: không đọc input / output.
SELECT status, estimated_cost::text AS estimated_cost, pause_requested_at, cancel_requested_at, created_at, finished_at
FROM agent_executions WHERE org_id = @org_id AND id = @id;

-- name: ListTaskStates :many
SELECT id, status, plan_key, parent_task_id, agent_id, depth
FROM agent_tasks WHERE org_id = @org_id AND execution_id = @execution_id
ORDER BY created_at, id LIMIT 64;

-- name: ListStepStates :many
-- Step của các task từ một step_seq trở đi (mỗi task một mốc), cho luồng sự kiện. Chỉ
-- định danh và trạng thái: không đọc request / response. Đi theo index (org_id, task_id, step_seq).
SELECT s.task_id, s.step_seq, s.kind, s.status, s.tool_name, s.approval_id, s.created_at
FROM execution_steps s
JOIN generate_subscripts(@task_ids::uuid[], 1) AS g(i)
  ON s.task_id = (@task_ids::uuid[])[g.i] AND s.step_seq >= (@from_seqs::int[])[g.i]
WHERE s.org_id = @org_id AND s.created_at >= @since
ORDER BY s.created_at, s.task_id, s.step_seq LIMIT @row_limit;

-- name: InsertPlanStep :exec
INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config)
VALUES (@org_id, @execution_id, @plan_key, @kind, @config);

-- name: StartPlanStep :execrows
UPDATE execution_plan_steps SET status = 'running', started_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND plan_key = @plan_key AND status = 'queued';

-- name: FinishPlanStep :execrows
-- Đóng một nút CHỜ. Nút chỉ "completed" khi nó đã chạy và đã chờ đủ thời gian của nó theo
-- đồng hồ của database (trừ 2 giây cho độ lệch với đồng hồ của nơi chạy kế hoạch): thứ
-- tự và thời điểm do nơi chạy kế hoạch đưa không được tin. Trigger của bảng giữ cùng luật
-- với mọi role. Nút approval không đóng bằng câu này (ClosePlanApproval).
UPDATE execution_plan_steps SET status = @status, finished_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND plan_key = @plan_key AND kind = 'wait' AND status IN ('queued','running')
  AND (@status::text <> 'completed'
       OR (status = 'running' AND started_at <= now() - make_interval(secs => GREATEST((config->>'seconds')::float8 - 2, 0))));

-- name: RequestPlanApproval :one
-- Điểm tạo approval của một nút approval (hàm trong database, migration 00033): nội dung
-- lấy từ cấu hình đã chụp của nút. NULL khi nút không ở trạng thái nhận approval.
SELECT COALESCE(plan_step_request_approval(@execution_id::uuid, @plan_key::text), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS approval_id;

-- name: PlanApprovalStatus :one
-- Trạng thái approval của một nút ('' khi chưa có).
SELECT COALESCE(plan_step_approval_status(@execution_id::uuid, @plan_key::text), '')::text AS status;

-- name: ClosePlanApproval :one
-- Kết thúc một nút approval (hàm trong database ghi cả approval lẫn nút) và trả về trạng
-- thái của nút: completed | failed | cancelled | timed_out; 'pending' khi approval còn chờ
-- và không có lý do dừng (không đổi gì); '' khi không phải nút approval.
SELECT COALESCE(plan_step_close_approval(@execution_id::uuid, @plan_key::text, @stop::text), '')::text AS result;

-- name: PlanNodeGatesOpen :one
-- Mọi cổng approval mà một nút của kế hoạch phụ thuộc (kể cả gián tiếp) đã mở.
SELECT plan_node_gates_open(@execution_id::uuid, @plan_key::text)::boolean AS open;

-- name: CloseOpenPlanApprovals :exec
-- Khi execution đóng: approval còn mở của các nút approval bị huỷ.
SELECT plan_close_open_approvals(@execution_id::uuid);

-- name: CountWaitingPlanSteps :one
-- Số nút approval đang chờ người duyệt của execution.
SELECT count(*) FROM execution_plan_steps
WHERE org_id = @org_id AND execution_id = @execution_id AND kind = 'approval' AND status = 'running';

-- name: SkipPlanStep :execrows
-- Nút không được chạy (một bước nó phụ thuộc không hoàn thành). Chỉ nút còn queued.
UPDATE execution_plan_steps SET status = 'skipped', finished_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND plan_key = @plan_key AND status = 'queued';

-- name: FinishOpenPlanSteps :exec
-- Khi execution đóng: nút chưa kết thúc đóng theo.
UPDATE execution_plan_steps SET status = @status, finished_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND status IN ('queued','running');

-- name: ListPlanSteps :many
SELECT plan_key, kind, status, config FROM execution_plan_steps
WHERE org_id = @org_id AND execution_id = @execution_id ORDER BY plan_key LIMIT 64;

-- name: TaskRetrySafe :one
-- Lần thử trước của một bước có chắc chắn chưa gây tác động không (migration 00036):
-- database quyết định, từ chỗ giữ của Gateway.
SELECT agent_task_retry_safe(@task_id)::boolean AS safe;
