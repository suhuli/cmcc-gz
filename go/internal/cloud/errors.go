package cloud

import "fmt"

// Kind 对应 Python 异常体系：用于上层区分处理方式。
type Kind int

const (
	KindAPI Kind = iota
	KindAuth
	KindNotFound
	KindConflict
	KindRateLimited
	KindNetwork
)

// APIError 是云端返回的业务错误或网络错误。
type APIError struct {
	Kind       Kind
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (code=%s)", e.Message, e.Code)
	}
	return e.Message
}

// IsKind 判断错误类型。
func IsKind(err error, k Kind) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Kind == k
}
