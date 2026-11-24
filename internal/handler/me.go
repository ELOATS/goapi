package handler

import (
	"net/http"

	"ELOATS/goapi/internal/auth"
	"ELOATS/goapi/internal/httpx"
)

// Me 返回当前调用方的身份和角色，方便排查"为什么是 403"。
func Me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, http.StatusUnauthorized, "authentication required")
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}
