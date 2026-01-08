package transport

import "errors"

// ErrUnimplemented can be returned by optional transport methods that are not supported.
var ErrUnimplemented = errors.New("unimplemented")
