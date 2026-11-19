// ApiError: lỗi của một lời gọi Management API. status = 0 khi không tới được máy chủ.
export interface ProblemLike { detail?: string; title?: string; code?: string }

export class ApiError extends Error {
  readonly status: number;
  readonly code: string | undefined;

  constructor(status: number, problem: ProblemLike | null) {
    super((problem && (problem.detail || problem.title)) || ('HTTP ' + status));
    this.name = 'ApiError';
    this.status = status;
    this.code = problem?.code;
  }
}
