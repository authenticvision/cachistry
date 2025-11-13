package httputil

import (
	"fmt"
	"io"
	"net/http"
)

type Error struct {
	StatusCode int
	Message    string
}

func (e Error) Error() string {
	return fmt.Sprintf("http status %d: %s", e.StatusCode, e.Message)
}

// ResponseAsError packs an http.Response into an error. It assumes the user
// has checked the status code already. It reads up to 4KiB of body as error
// message and closes the body.
func ResponseAsError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	_ = resp.Body.Close()
	return &Error{
		StatusCode: resp.StatusCode,
		Message:    string(msg),
	}
}
