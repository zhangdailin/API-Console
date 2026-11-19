package errors

import (
	stderrors "errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestLocalRequestMessagesTrustOnlyTypedAllowlistedReasons(t *testing.T) {
	for reason, want := range localRequestMessages {
		err := NewLocalRequestError(reason)
		var app *AppError
		if !stderrors.As(err, &app) || app.HTTPStatus != http.StatusBadRequest || app.Code != "invalid_request_error" {
			t.Fatal("local validation lost its application error contract")
		}
		app.Message = "private-upstream-body-and-token"
		got, ok := LocalRequestMessage(fmt.Errorf("private-context: %w", err))
		if !ok || got != want || strings.Contains(got, "private") {
			t.Fatal("local validation trusted mutable or wrapped error text")
		}
		for _, forged := range []error{stderrors.New(want), New("invalid_request_error", want, 400), nil} {
			if _, ok := LocalRequestMessage(forged); ok {
				t.Fatal("untyped provider text opted into trusted local validation")
			}
		}
	}
	if got, ok := LocalRequestMessage(NewLocalRequestError("private-unknown-reason")); !ok || got != localRequestMessages["warp_request_shape"] {
		t.Fatal("unknown local reason was not reduced to a fixed fallback")
	}
	if StatusForCategory("request_validation") != 400 || !strings.Contains(CategoryMessage("request_validation"), "gateway rejected") {
		t.Fatal("local error category is ambiguous or has the wrong status")
	}
}
