// Package httpx 提供 JSON 编解码与统一错误响应。
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

type ErrorBody struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func Error(w http.ResponseWriter, r *http.Request, status int, msg string) {
	JSON(w, status, ErrorBody{Error: msg, RequestID: middleware.GetReqID(r.Context())})
}

// Decode 限制请求体大小、拒绝未知字段，并要求只包含一个 JSON 对象。
func Decode(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return fmt.Errorf("request body must not exceed %d bytes", maxErr.Limit)
		case errors.Is(err, io.EOF):
			return errors.New("request body must not be empty")
		default:
			return fmt.Errorf("invalid JSON: %w", err)
		}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}
