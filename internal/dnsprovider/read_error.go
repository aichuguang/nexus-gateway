package dnsprovider

import "errors"

// ReadUnavailableError is emitted only for explicitly read-only provider
// actions. Never wrap an ambiguous mutation in this type.
type ReadUnavailableError struct {
	Provider string
	Reason   string
}

func (e *ReadUnavailableError) Error() string {
	return e.Provider + " 读取暂时不可用：" + e.Reason
}

func RetryableRead(err error) bool {
	var unavailable *ReadUnavailableError
	return errors.As(err, &unavailable)
}
