package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// Slug là định danh của server này trong Registry, và là đích (aud) mà ngữ cảnh
// do Gateway ký phải mang.
const Slug = "github"

// Version là phiên bản server tự khai trong Registry.
const Version = "0.1.0"

type listRepositoriesInput struct{}

type listRepositoriesOutput struct {
	Repositories []string `json:"repositories" jsonschema:"các repository server này được phép đọc, dạng owner/repo"`
	Total        int      `json:"total"`
}

type listPullRequestsInput struct {
	Owner   string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo    string `json:"repo" jsonschema:"tên repository"`
	State   string `json:"state,omitempty" jsonschema:"mặc định open"`
	Page    int    `json:"page,omitempty" jsonschema:"số trang, bắt đầu từ 1; dùng next_page của lần gọi trước"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"số mục mỗi trang (1-50, mặc định 20)"`
}

type listPullRequestsOutput struct {
	PullRequests []PullRequest `json:"pull_requests"`
	PageInfo
}

type getPullRequestInput struct {
	Owner  string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo   string `json:"repo" jsonschema:"tên repository"`
	Number int64  `json:"number" jsonschema:"số của pull request"`
}

type listIssuesInput struct {
	Owner   string   `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo    string   `json:"repo" jsonschema:"tên repository"`
	State   string   `json:"state,omitempty" jsonschema:"mặc định open"`
	Labels  []string `json:"labels,omitempty" jsonschema:"chỉ lấy issue mang TẤT CẢ các label này (tối đa 10)"`
	Page    int      `json:"page,omitempty" jsonschema:"số trang, bắt đầu từ 1; dùng next_page của lần gọi trước"`
	PerPage int      `json:"per_page,omitempty" jsonschema:"số mục mỗi trang (1-50, mặc định 20)"`
}

type listIssuesOutput struct {
	Issues              []Issue `json:"issues"`
	PullRequestsOmitted int     `json:"pull_requests_omitted,omitempty" jsonschema:"số mục của trang này là pull request nên đã bị lọc ra; trang có thể ít mục hơn per_page"`
	PageInfo
}

type getIssueInput struct {
	Owner  string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo   string `json:"repo" jsonschema:"tên repository"`
	Number int64  `json:"number" jsonschema:"số của issue"`
}

type listWorkflowRunsInput struct {
	Owner   string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo    string `json:"repo" jsonschema:"tên repository"`
	Branch  string `json:"branch,omitempty" jsonschema:"chỉ lấy lần chạy của branch này; không nhận refs/pull/*"`
	Status  string `json:"status,omitempty" jsonschema:"chỉ lấy lần chạy có trạng thái hoặc kết luận này"`
	Page    int    `json:"page,omitempty" jsonschema:"số trang, bắt đầu từ 1; dùng next_page của lần gọi trước"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"số mục mỗi trang (1-50, mặc định 20)"`
}

type listWorkflowRunsOutput struct {
	WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	Total        int           `json:"total" jsonschema:"tổng số lần chạy khớp bộ lọc theo GitHub (khi có lọc, GitHub trả tối đa 1000 kết quả)"`
	PageInfo
}

type getWorkflowRunInput struct {
	Owner string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo  string `json:"repo" jsonschema:"tên repository"`
	RunID int64  `json:"run_id" jsonschema:"id của lần chạy workflow (trường id của list_workflow_runs)"`
}

type listCommitsInput struct {
	Owner   string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo    string `json:"repo" jsonschema:"tên repository"`
	Ref     string `json:"ref,omitempty" jsonschema:"branch, tag hoặc SHA để bắt đầu liệt kê; mặc định là branch mặc định của repository. Không nhận refs/pull/*. Một SHA có thể thuộc mạng fork của repository (commit của fork, không nằm trên branch nào của repository này)"`
	Since   string `json:"since,omitempty" jsonschema:"chỉ lấy commit từ thời điểm này, RFC 3339"`
	Until   string `json:"until,omitempty" jsonschema:"chỉ lấy commit trước thời điểm này, RFC 3339"`
	Page    int    `json:"page,omitempty" jsonschema:"số trang, bắt đầu từ 1; dùng next_page của lần gọi trước"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"số mục mỗi trang (1-50, mặc định 20)"`
}

type listCommitsOutput struct {
	Commits []Commit `json:"commits"`
	PageInfo
}

type getCommitStatusInput struct {
	Owner string `json:"owner" jsonschema:"tài khoản hoặc tổ chức sở hữu repository"`
	Repo  string `json:"repo" jsonschema:"tên repository"`
	SHA   string `json:"sha" jsonschema:"SHA đầy đủ của commit (40 hoặc 64 ký tự hex viết thường), ví dụ head_sha của pull request. GitHub trả lời cho mọi commit trong mạng fork của repository, kể cả commit chỉ có ở fork của người ngoài"`
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// Giới hạn đặt vào input schema của từng trường. Schema là thứ Registry ghim và
// Gateway dùng để kiểm arguments; Client vẫn kiểm lại các giới hạn này.
type bound struct {
	maxLength int
	pattern   string
	enum      []any
	min, max  *float64
	// Cho trường mảng: số phần tử tối đa và giới hạn của từng phần tử.
	maxItems int
	items    *bound
}

func intRange(lo, hi int) bound {
	return bound{min: jsonschema.Ptr(float64(lo)), max: jsonschema.Ptr(float64(hi))}
}

func enumOf(values ...string) bound {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return bound{enum: out}
}

// RFC 3339 với múi giờ tường minh; time.Parse kiểm lại phần còn lại.
const timePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`

var (
	ownerBound   = bound{maxLength: 39, pattern: ownerRe.String()}
	repoBound    = bound{maxLength: 100, pattern: repoRe.String()}
	stateBound   = enumOf("open", "closed", "all")
	numberBound  = bound{min: jsonschema.Ptr(1.0), max: jsonschema.Ptr(float64(maxNumber))}
	pageBound    = intRange(1, maxPage)
	perPageBound = intRange(1, maxPerPage)
	refBound     = bound{maxLength: 255, pattern: refRe.String()}
	timeBound    = bound{maxLength: 40, pattern: timePattern}
)

func (b bound) apply(p *jsonschema.Schema) {
	if b.maxLength > 0 {
		p.MinLength, p.MaxLength = jsonschema.Ptr(1), jsonschema.Ptr(b.maxLength)
	}
	p.Pattern, p.Enum, p.Minimum, p.Maximum = b.pattern, b.enum, b.min, b.max
	if b.items != nil {
		if p.Items == nil {
			panic("github: input schema: giới hạn phần tử đặt trên trường không phải mảng")
		}
		p.MaxItems = jsonschema.Ptr(b.maxItems)
		b.items.apply(p.Items)
	}
}

// inputSchema suy schema từ kiểu Go rồi thêm giới hạn cho từng trường. Mọi trường
// của In phải có giới hạn: trường thiếu làm server không khởi động được.
func inputSchema[In any](bounds map[string]bound) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic("github: input schema: " + err.Error())
	}
	if len(s.Properties) != len(bounds) {
		panic("github: input schema: mỗi trường phải có đúng một giới hạn")
	}
	for name, b := range bounds {
		p := s.Properties[name]
		if p == nil {
			panic("github: input schema: không có trường " + name)
		}
		b.apply(p)
	}
	return s
}

func parseTime(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New(field + " phải là thời điểm RFC 3339, ví dụ 2026-11-18T08:00:00Z")
	}
	return t, nil
}

// NewServer dựng MCP server với chín tool chỉ đọc.
func NewServer(client *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hq-github", Version: Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_repositories",
		Description: "Liệt kê các repository GitHub mà server này được cấu hình để đọc (dạng owner/repo). Các tool khác chỉ làm việc trên những repository này. Không gọi GitHub. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listRepositoriesInput](map[string]bound{}),
	}, func(context.Context, *mcp.CallToolRequest, listRepositoriesInput) (*mcp.CallToolResult, listRepositoriesOutput, error) {
		repos := client.Repositories()
		return nil, listRepositoriesOutput{Repositories: repos, Total: len(repos)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_pull_requests",
		Description: "Liệt kê pull request của một repository, mới tạo trước, lọc theo trạng thái. Không trả mô tả hay diff. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listPullRequestsInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "state": stateBound, "page": pageBound, "per_page": perPageBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listPullRequestsInput) (*mcp.CallToolResult, listPullRequestsOutput, error) {
		items, info, err := client.PullRequests(ctx, in.Owner, in.Repo, in.State, Page{Page: in.Page, PerPage: in.PerPage})
		if err != nil {
			return nil, listPullRequestsOutput{}, err
		}
		return nil, listPullRequestsOutput{PullRequests: items, PageInfo: info}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_pull_request",
		Description: "Đọc một pull request: tiêu đề, mô tả (cắt ở 8 KiB), trạng thái, số commit / file / dòng thêm / dòng xoá và tóm tắt review theo từng người review. Tóm tắt review không phải kết quả branch protection và không dùng để quyết định merge. Không trả diff hay nội dung file. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[getPullRequestInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "number": numberBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getPullRequestInput) (*mcp.CallToolResult, PullRequestDetail, error) {
		out, err := client.PullRequest(ctx, in.Owner, in.Repo, in.Number)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_issues",
		Description: "Liệt kê issue của một repository, mới tạo trước, lọc theo trạng thái và label. Pull request bị lọc ra. Không trả nội dung issue. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listIssuesInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "state": stateBound,
			"labels": {maxItems: maxLabelFilters, items: &bound{maxLength: 50, pattern: labelRe.String()}},
			"page":   pageBound, "per_page": perPageBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listIssuesInput) (*mcp.CallToolResult, listIssuesOutput, error) {
		items, info, omitted, err := client.Issues(ctx, in.Owner, in.Repo, in.State, in.Labels, Page{Page: in.Page, PerPage: in.PerPage})
		if err != nil {
			return nil, listIssuesOutput{}, err
		}
		return nil, listIssuesOutput{Issues: items, PullRequestsOmitted: omitted, PageInfo: info}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_issue",
		Description: "Đọc một issue: tiêu đề, nội dung (cắt ở 8 KiB), trạng thái, label, người được giao và số comment. Không trả comment. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[getIssueInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "number": numberBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getIssueInput) (*mcp.CallToolResult, IssueDetail, error) {
		out, err := client.Issue(ctx, in.Owner, in.Repo, in.Number)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_workflow_runs",
		Description: "Liệt kê lần chạy workflow GitHub Actions của một repository, mới nhất trước, lọc theo branch và trạng thái. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listWorkflowRunsInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "branch": refBound, "status": enumOf(RunStatuses...),
			"page": pageBound, "per_page": perPageBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listWorkflowRunsInput) (*mcp.CallToolResult, listWorkflowRunsOutput, error) {
		items, info, total, err := client.WorkflowRuns(ctx, in.Owner, in.Repo, in.Branch, in.Status, Page{Page: in.Page, PerPage: in.PerPage})
		if err != nil {
			return nil, listWorkflowRunsOutput{}, err
		}
		return nil, listWorkflowRunsOutput{WorkflowRuns: items, Total: total, PageInfo: info}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_workflow_run",
		Description: "Đọc một lần chạy workflow GitHub Actions kèm danh sách job của lần thử gần nhất (tối đa 100), kết luận của từng job và tên các step thất bại. Không trả log của job. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[getWorkflowRunInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "run_id": numberBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getWorkflowRunInput) (*mcp.CallToolResult, WorkflowRunDetail, error) {
		out, err := client.WorkflowRun(ctx, in.Owner, in.Repo, in.RunID)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_commits",
		Description: "Liệt kê commit của một repository, mới nhất trước, bắt đầu từ một branch, tag hoặc SHA, lọc theo khoảng thời gian. Thông điệp commit cắt ở 512 byte; không trả email tác giả, diff hay nội dung file. Bắt đầu từ một SHA thì commit có thể thuộc fork của repository. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listCommitsInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "ref": refBound, "since": timeBound, "until": timeBound,
			"page": pageBound, "per_page": perPageBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listCommitsInput) (*mcp.CallToolResult, listCommitsOutput, error) {
		since, err := parseTime("since", in.Since)
		if err != nil {
			return nil, listCommitsOutput{}, err
		}
		until, err := parseTime("until", in.Until)
		if err != nil {
			return nil, listCommitsOutput{}, err
		}
		items, info, err := client.Commits(ctx, in.Owner, in.Repo, in.Ref, since, until, Page{Page: in.Page, PerPage: in.PerPage})
		if err != nil {
			return nil, listCommitsOutput{}, err
		}
		return nil, listCommitsOutput{Commits: items, PageInfo: info}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_commit_status",
		Description: "Đọc trạng thái kiểm tra của một commit theo SHA đầy đủ: combined status của các commit status (tối đa 50) và tóm tắt check run (tối đa 100: tên, trạng thái, kết luận). Commit có thể thuộc fork của repository; status và check run do hệ thống bên ngoài báo. Không trả log hay output của check. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[getCommitStatusInput](map[string]bound{
			"owner": ownerBound, "repo": repoBound, "sha": {maxLength: 64, pattern: shaRe.String()}}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getCommitStatusInput) (*mcp.CallToolResult, CommitStatus, error) {
		out, err := client.CommitStatus(ctx, in.Owner, in.Repo, in.SHA)
		return nil, out, err
	})
	return s
}

// DirectTools là các tool client bên ngoài gọi trực tiếp được qua Gateway (serve.Direct);
// tool khác chỉ chạy trong một execution của Agent. Chỉ hai tool không trả văn bản do người
// ngoài viết: danh sách repo trong allowlist và trạng thái check của một commit. Tiêu đề và
// thân PR / issue, commit message, tên lần chạy workflow (lấy từ commit message) đều do bất
// kỳ ai có quyền đẩy code hay mở PR viết, là nguồn prompt injection: chỉ chạy trong execution.
var DirectTools = []string{"list_repositories", "get_commit_status"}

// Handler trả về HTTP handler của server: chỉ nhận request mang ngữ cảnh do Gateway ký cho
// đúng server này; một tài khoản GitHub (token + allowlist) thuộc về đúng một organization
// (orgID); ngữ cảnh discovery chỉ đọc được danh sách tool. Xem internal/integration/serve.
func Handler(client *Client, verifier *signedctx.Verifier, orgID uuid.UUID) http.Handler {
	return serve.Handler(Slug, verifier, orgID, func() *mcp.Server { return NewServer(client) },
		serve.Direct(DirectTools...))
}
