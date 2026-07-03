package tracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxJSONResponseBytes = 16 << 20

var ErrJSONResponseTooLarge = errors.New("tracker JSON response exceeded maximum size")

// DecodeJSONResponse decodes a successful tracker response without allowing a
// misbehaving endpoint to stream unbounded JSON into the worker.
func DecodeJSONResponse(resp *http.Response, out any) error {
	if resp == nil || resp.Body == nil {
		return errors.New("tracker JSON response body is nil")
	}
	if resp.ContentLength > maxJSONResponseBytes {
		return fmt.Errorf("%w: limit=%d content_length=%d", ErrJSONResponseTooLarge, maxJSONResponseBytes, resp.ContentLength)
	}
	var body bytes.Buffer
	n, err := body.ReadFrom(io.LimitReader(resp.Body, maxJSONResponseBytes+1))
	if err != nil {
		return err
	}
	if n > maxJSONResponseBytes {
		return fmt.Errorf("%w: limit=%d", ErrJSONResponseTooLarge, maxJSONResponseBytes)
	}
	return json.NewDecoder(bytes.NewReader(body.Bytes())).Decode(out)
}
