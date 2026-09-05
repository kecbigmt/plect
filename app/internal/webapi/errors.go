package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
)

// errorClass carries the HTTP status and the ApiError category a
// service.Error code maps to. Every code service.Error currently defines is
// listed here — this table is the "common errors" contract's single source
// of truth, shared by every Web API operation rather than reimplemented per
// handler.
type errorClass struct {
	status   int
	category webapiv1.ErrorCategory
}

var errorClasses = map[string]errorClass{
	service.ErrSessionNotFound: {http.StatusNotFound, webapiv1.ErrorCategoryNotFound},

	service.ErrInvalidURL:   {http.StatusBadRequest, webapiv1.ErrorCategoryValidation},
	service.ErrInvalidTag:   {http.StatusBadRequest, webapiv1.ErrorCategoryValidation},
	service.ErrInvalidInput: {http.StatusBadRequest, webapiv1.ErrorCategoryValidation},
	// A fixed server-side policy decision evaluated before any service call
	// is attempted (the resolved resource is outside the configured
	// allowlist/session guard) — a validation-class rejection of the
	// request, not a conflict with the session's existing relationships.
	service.ErrRepoNotAllowed: {http.StatusForbidden, webapiv1.ErrorCategoryValidation},

	service.ErrHasChildren:        {http.StatusConflict, webapiv1.ErrorCategoryConflict},
	service.ErrRelationNotAllowed: {http.StatusConflict, webapiv1.ErrorCategoryConflict},
	service.ErrChildCapExceeded:   {http.StatusConflict, webapiv1.ErrorCategoryConflict},
	service.ErrChildUpInProgress:  {http.StatusConflict, webapiv1.ErrorCategoryConflict},

	// not_attachable/not_produced/not_capturable report the operation cannot
	// proceed given the session's current lifecycle state, not that the
	// request itself is malformed — a 409, like the relationship conflicts
	// above, rather than a 4xx validation failure or a 5xx server fault.
	service.ErrNotAttachable: {http.StatusConflict, webapiv1.ErrorCategoryExecution},
	service.ErrNotProduced:   {http.StatusConflict, webapiv1.ErrorCategoryExecution},
	service.ErrNotCapturable: {http.StatusConflict, webapiv1.ErrorCategoryExecution},

	service.ErrExecutionFailed: {http.StatusInternalServerError, webapiv1.ErrorCategoryExecution},
}

// apiError converts any error into the (HTTP status, JSON body) pair the
// "common errors" contract promises. A *service.Error uses its code's
// documented class; every other error (a store I/O failure, a bug) is an
// unclassified execution failure — 500, never a guess at 4xx.
func apiError(err error) (int, any) {
	svcErr, ok := err.(*service.Error)
	if !ok {
		return http.StatusInternalServerError, webapiv1.ExecutionError{
			Category: webapiv1.ExecutionErrorCategoryExecution,
			Code:     webapiv1.ExecutionFailed,
			Message:  err.Error(),
		}
	}

	class, ok := errorClasses[svcErr.Code]
	if !ok {
		// A code this table hasn't classified yet is this package's bug, not
		// the caller's — surface it as an unclassified execution failure
		// rather than panic or silently mislabel it as some other category.
		return http.StatusInternalServerError, webapiv1.ExecutionError{
			Category: webapiv1.ExecutionErrorCategoryExecution,
			Code:     webapiv1.ExecutionFailed,
			Message:  svcErr.Message,
		}
	}

	switch class.category {
	case webapiv1.ErrorCategoryNotFound:
		return class.status, webapiv1.NotFoundError{
			Category: webapiv1.NotFoundErrorCategoryNotFound,
			Code:     webapiv1.NotFoundErrorCode(svcErr.Code),
			Message:  svcErr.Message,
		}
	case webapiv1.ErrorCategoryValidation:
		return class.status, webapiv1.ValidationError{
			Category: webapiv1.ValidationErrorCategoryValidation,
			Code:     webapiv1.ValidationErrorCode(svcErr.Code),
			Message:  svcErr.Message,
		}
	case webapiv1.ErrorCategoryConflict:
		return class.status, webapiv1.ConflictError{
			Category: webapiv1.ConflictErrorCategoryConflict,
			Code:     webapiv1.ConflictErrorCode(svcErr.Code),
			Message:  svcErr.Message,
		}
	default: // webapiv1.ErrorCategoryExecution
		return class.status, webapiv1.ExecutionError{
			Category: webapiv1.ExecutionErrorCategoryExecution,
			Code:     webapiv1.ExecutionErrorCode(svcErr.Code),
			Message:  svcErr.Message,
		}
	}
}

// DecodeApiError is the read side of the ApiError tagged union: given any
// error response body this contract can produce, it peeks at the
// `category` discriminant and decodes into the matching concrete leaf
// type — the operation a generic client needs (log/retry on category
// without knowing every service.Error code) and that oapi-codegen's
// types-only generation does not itself provide, since nothing in this
// slice's OpenAPI document declares a single oneOf schema for the four
// leaves (each operation's responses are already discriminated by HTTP
// status instead; see web/api/README.md).
func DecodeApiError(raw []byte) (any, error) {
	var base webapiv1.ApiError
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, fmt.Errorf("decode ApiError envelope: %w", err)
	}
	switch base.Category {
	case webapiv1.ErrorCategoryNotFound:
		var v webapiv1.NotFoundError
		err := json.Unmarshal(raw, &v)
		return v, err
	case webapiv1.ErrorCategoryValidation:
		var v webapiv1.ValidationError
		err := json.Unmarshal(raw, &v)
		return v, err
	case webapiv1.ErrorCategoryConflict:
		var v webapiv1.ConflictError
		err := json.Unmarshal(raw, &v)
		return v, err
	case webapiv1.ErrorCategoryExecution:
		var v webapiv1.ExecutionError
		err := json.Unmarshal(raw, &v)
		return v, err
	default:
		return nil, fmt.Errorf("ApiError: unrecognized category %q", base.Category)
	}
}
