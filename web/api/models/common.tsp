import "@typespec/http";

using Http;

namespace Plecture.WebApi;

/// A bounded, non-paged collection envelope. `count` is the number of
/// `items` actually returned, not a separate server-computed total — this
/// slice's list endpoint has no pagination cursor yet.
model ListResponse<T> {
  items: T[];
  count: int32;
}

/// The wire discriminant every `ApiError` variant carries. Grouped from the
/// existing flat `service.Error` codes by the failure class an HTTP client
/// reacts to, not invented independently of them.
enum ErrorCategory {
  validation,
  notFound: "not_found",
  conflict,
  execution,
}

/// Every error response body is one of the variants below, tagged by
/// `category` — the coarser class a generic HTTP client branches on without
/// knowing every individual `service.Error.Code`, which each variant carries
/// as its own narrower `code`.
@discriminator("category")
model ApiError {
  category: ErrorCategory;
  message: string;
}

/// The requested session (or another named resource) does not exist.
model NotFoundError extends ApiError {
  category: ErrorCategory.notFound;
  code: "session_not_found";
}

/// The request itself was malformed or failed input validation before any
/// service call was attempted.
model ValidationError extends ApiError {
  category: ErrorCategory.validation;
  code: "invalid_url" | "invalid_tag" | "invalid_input" | "repo_not_allowed";
}

/// The request was well-formed but conflicts with the session's current
/// relationships or an in-progress operation.
model ConflictError extends ApiError {
  category: ErrorCategory.conflict;
  code: "has_children" | "relation_not_allowed" | "child_cap_exceeded" | "child_up_in_progress";
}

/// The service call was attempted and failed while running.
model ExecutionError extends ApiError {
  category: ErrorCategory.execution;
  code: "execution_failed" | "not_attachable" | "not_produced" | "not_capturable";
}
