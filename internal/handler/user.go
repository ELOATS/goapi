// Package handler 是 HTTP 层：解析请求、调用业务、映射错误。
package handler

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"example.com/goapi/internal/domain"
	"example.com/goapi/internal/httpx"
)

// UserService 在使用方定义接口（Go 惯例），实现在 service 包。
type UserService interface {
	Get(ctx context.Context, id string) (domain.User, error)
	Create(ctx context.Context, name, email string) (domain.User, error)
	List(ctx context.Context, p domain.ListUsersParams) (domain.UserPage, error)
}

type UserHandler struct {
	svc      UserService
	maxBytes int64
	cursors  *CursorCodec
}

func NewUserHandler(svc UserService, maxBytes int64, cursors *CursorCodec) *UserHandler {
	if cursors == nil {
		panic("handler: cursors is required")
	}
	return &UserHandler{svc: svc, maxBytes: maxBytes, cursors: cursors}
}

type createUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (req *createUserRequest) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)
	if req.Name == "" || len(req.Name) > 100 {
		return errors.New("name is required and must be at most 100 characters")
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		return errors.New("email is invalid")
	}
	return nil
}

func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := httpx.Decode(w, r, &req, h.maxBytes); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	u, err := h.svc.Create(r.Context(), req.Name, req.Email)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/users/"+u.ID)
	httpx.JSON(w, http.StatusCreated, u)
}

func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, u)
}

type listUsersResponse struct {
	Items      []domain.User `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// List 处理 GET /api/v1/users?limit=20&cursor=<next_cursor>
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var p domain.ListUsersParams

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > domain.MaxPageSize {
			httpx.Error(w, r, http.StatusBadRequest,
				"limit must be an integer between 1 and "+strconv.Itoa(domain.MaxPageSize))
			return
		}
		p.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		c, err := h.cursors.Decode(v)
		if err != nil {
			httpx.Error(w, r, http.StatusBadRequest, "invalid cursor")
			return
		}
		p.After = &c
	}

	page, err := h.svc.List(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := listUsersResponse{Items: page.Items}
	if page.Next != nil {
		resp.NextCursor = h.cursors.Encode(*page.Next)
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// fail 集中把领域错误映射为 HTTP 状态码；未知错误不向客户端泄露细节。
func (h *UserHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		httpx.Error(w, r, http.StatusNotFound, "user not found")
	case errors.Is(err, domain.ErrConflict):
		httpx.Error(w, r, http.StatusConflict, "user already exists")
	case errors.Is(err, domain.ErrInvalidCursor):
		httpx.Error(w, r, http.StatusBadRequest, "invalid cursor")
	case errors.Is(err, context.DeadlineExceeded):
		httpx.Error(w, r, http.StatusGatewayTimeout, "request timed out")
	default:
		httpx.Error(w, r, http.StatusInternalServerError, "internal server error")
	}
}
