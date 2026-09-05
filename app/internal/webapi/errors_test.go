package webapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
)

// Every service.Error code this package knows about must classify to a
// distinct (status, category) pair — the "common errors" contract's whole
// point is that a caller can trust this table rather than special-case each
// service call, so a code with no listed case here is this test's failure,
// not just apiError's.
func TestApiError_ClassifiesEveryKnownServiceErrorCode(t *testing.T) {
	tests := []struct {
		code     string
		status   int
		category webapiv1.ErrorCategory
	}{
		{service.ErrSessionNotFound, http.StatusNotFound, webapiv1.ErrorCategoryNotFound},
		{service.ErrInvalidURL, http.StatusBadRequest, webapiv1.ErrorCategoryValidation},
		{service.ErrInvalidTag, http.StatusBadRequest, webapiv1.ErrorCategoryValidation},
		{service.ErrInvalidInput, http.StatusBadRequest, webapiv1.ErrorCategoryValidation},
		{service.ErrRepoNotAllowed, http.StatusForbidden, webapiv1.ErrorCategoryValidation},
		{service.ErrHasChildren, http.StatusConflict, webapiv1.ErrorCategoryConflict},
		{service.ErrRelationNotAllowed, http.StatusConflict, webapiv1.ErrorCategoryConflict},
		{service.ErrChildCapExceeded, http.StatusConflict, webapiv1.ErrorCategoryConflict},
		{service.ErrChildUpInProgress, http.StatusConflict, webapiv1.ErrorCategoryConflict},
		{service.ErrNotAttachable, http.StatusConflict, webapiv1.ErrorCategoryExecution},
		{service.ErrNotProduced, http.StatusConflict, webapiv1.ErrorCategoryExecution},
		{service.ErrNotCapturable, http.StatusConflict, webapiv1.ErrorCategoryExecution},
		{service.ErrExecutionFailed, http.StatusInternalServerError, webapiv1.ErrorCategoryExecution},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			status, body := apiError(&service.Error{Code: tt.code, Message: "boom"})
			if status != tt.status {
				t.Errorf("status = %d, want %d", status, tt.status)
			}
			category := categoryOf(t, body)
			if category != tt.category {
				t.Errorf("category = %q, want %q", category, tt.category)
			}
		})
	}
}

func TestApiError_UnknownServiceErrorCodeIsAnUnclassifiedExecutionFailure(t *testing.T) {
	status, body := apiError(&service.Error{Code: "not_a_real_code", Message: "boom"})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	exec, ok := body.(webapiv1.ExecutionError)
	if !ok {
		t.Fatalf("body = %#v, want ExecutionError", body)
	}
	if exec.Code != webapiv1.ExecutionFailed {
		t.Errorf("Code = %q, want execution_failed", exec.Code)
	}
}

// A plain error (store I/O failure, anything not raised as *service.Error)
// is a 500: this package never guesses at a 4xx for an error the service
// layer did not itself classify.
func TestApiError_PlainErrorIsA500ExecutionFailure(t *testing.T) {
	status, body := apiError(errors.New("disk full"))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	exec, ok := body.(webapiv1.ExecutionError)
	if !ok {
		t.Fatalf("body = %#v, want ExecutionError", body)
	}
	if exec.Message != "disk full" {
		t.Errorf("Message = %q, want the underlying error text", exec.Message)
	}
}

// categoryOf reads the `category` field off any of the four ApiError leaf
// structs by type-switching — the same dispatch a generic client would need
// if it wanted to branch on category without knowing every concrete Go type,
// proving the discriminated union is usable that way, not just decodable.
func categoryOf(t *testing.T, body any) webapiv1.ErrorCategory {
	t.Helper()
	switch v := body.(type) {
	case webapiv1.NotFoundError:
		return webapiv1.ErrorCategory(v.Category)
	case webapiv1.ValidationError:
		return webapiv1.ErrorCategory(v.Category)
	case webapiv1.ConflictError:
		return webapiv1.ErrorCategory(v.Category)
	case webapiv1.ExecutionError:
		return webapiv1.ErrorCategory(v.Category)
	default:
		t.Fatalf("body = %#v, want one of the ApiError leaf types", body)
		return ""
	}
}
