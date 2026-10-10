package cloud

import (
	"errors"
	"fmt"
)

// Kind 是错误分类，供上层决定如何处理（重新登录、返回 404、重试等）。
type Kind int

const (
	KindAPI Kind = iota
	KindAuth
	KindNotFound
	KindConflict
	KindRateLimited
	KindNetwork
)

func (k Kind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindRateLimited:
		return "rate_limited"
	case KindNetwork:
		return "network"
	default:
		return "api"
	}
}

// APIError 是云端返回的业务错误或网络错误。
type APIError struct {
	Kind       Kind
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.HTTPStatus != 0:
		return fmt.Sprintf("%s (code=%s, http=%d)", e.Message, e.Code, e.HTTPStatus)
	case e.Code != "":
		return fmt.Sprintf("%s (code=%s)", e.Message, e.Code)
	case e.HTTPStatus != 0:
		return fmt.Sprintf("%s (http=%d)", e.Message, e.HTTPStatus)
	}
	return e.Message
}

// IsKind 判断错误类型，支持被 fmt.Errorf("%w") 包装过的错误。
func IsKind(err error, k Kind) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Kind == k
}

// UserMessage 返回适合直接展示给用户的错误文本。
func UserMessage(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// retryable 表示请求没有被服务端执行或执行失败，可以安全重试。
func retryable(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch {
	case ae.Kind == KindNetwork, ae.Kind == KindRateLimited:
		return true
	case ae.HTTPStatus == 429, ae.HTTPStatus == 502, ae.HTTPStatus == 503, ae.HTTPStatus == 504:
		return true
	case ae.HTTPStatus >= 500:
		return true
	}
	return false
}

// rejectedBeforeExecution 表示服务端明确拒绝（限流/网关错误），写操作也可以重试。
func rejectedBeforeExecution(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Kind == KindRateLimited || ae.HTTPStatus == 429 || ae.HTTPStatus == 502 || ae.HTTPStatus == 503
}

func statusError(code, msg string) *APIError {
	e := &APIError{Kind: KindAPI, Code: code, Message: msg}
	switch code {
	case "401", "403", "200000401", "200000413":
		e.Kind = KindAuth
	case "404", "200000404":
		e.Kind = KindNotFound
	case "409", "200000409":
		e.Kind = KindConflict
	case "429", "200000429":
		e.Kind = KindRateLimited
	}
	return e
}
