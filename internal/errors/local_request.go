package errors

import (
	stderrors "errors"
	"net/http"
)

// Local validation messages are an allowlist, not provider response text. The
// shared handler must never expose arbitrary AppError messages as trusted text.
var localRequestMessages = map[string]string{
	"doubao_attachment":  "Doubao attachment is invalid, too large, unavailable, or expired. Upload it with the same API key and try again.",
	"cline_image":        "Cline requires an HTTP(S) image URL or a valid Base64 PNG, JPEG, GIF, or WebP image. Use at most 8 images and 16 MiB of inline image data per request.",
	"workbuddy_image":    "WorkBuddy requires an HTTP(S) image URL or a valid Base64 PNG, JPEG, GIF, or WebP image. Use at most 8 images and 16 MiB of inline image data per request.",
	"warp_request_shape": "The gateway rejected this Warp request during local validation: unsupported request fields or conversation shape. No upstream generation was made.",
	"warp_dialog":        "The gateway rejected this Warp conversation during local validation: expected complete alternating user and assistant turns followed by a user turn. No upstream generation was made.",
	"warp_user_turn":     "The gateway rejected the current Warp user turn: provide nonempty text or a valid tool result. No upstream generation was made.",
	"warp_image":         "Warp accepts up to 8 inline Base64 PNG or JPEG images in user messages, with a combined decoded size of at most 512 KiB. No upstream generation was made.",
	"warp_image_model":   "The selected Warp model does not support image input. Select a model with image support. No upstream generation was made.",
	"warp_tool_choice":   "The gateway rejected this Warp tool choice: only auto and none are supported. No upstream generation was made.",
	"warp_model_catalog": "The gateway could not validate this exact Warp model against the account's fresh catalog. Refresh the model catalog and check the model selection. No upstream generation was made.",
}

type localRequestError struct {
	app    *AppError
	reason string
}

func (e *localRequestError) Error() string { return e.app.Error() }
func (e *localRequestError) Unwrap() error { return e.app }

// NewLocalRequestError marks a gateway-owned rejection before any generation.
// Unknown reasons collapse to a fixed message and can never inject client data.
func NewLocalRequestError(reason string) error {
	if _, ok := localRequestMessages[reason]; !ok {
		reason = "warp_request_shape"
	}
	return &localRequestError{app: New("invalid_request_error", localRequestMessages[reason], http.StatusBadRequest), reason: reason}
}

// LocalRequestMessage accepts only the private typed error created above. A raw
// upstream error or an AppError with the same message cannot opt into this path.
func LocalRequestMessage(err error) (string, bool) {
	var local *localRequestError
	if !stderrors.As(err, &local) || local == nil {
		return "", false
	}
	message, ok := localRequestMessages[local.reason]
	return message, ok
}
