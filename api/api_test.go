package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestSpecIsValid(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("openapi.yaml is invalid: %v", err)
	}
	seen := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "" {
				t.Errorf("%s %s has no operationId", method, path)
			}
			if seen[op.OperationID] {
				t.Errorf("duplicate operationId %q", op.OperationID)
			}
			seen[op.OperationID] = true
		}
	}
}

func TestHandlers(t *testing.T) {
	rec := httptest.NewRecorder()
	SpecHandler(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/yaml") ||
		!strings.Contains(rec.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("spec handler: code=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	UIHandler(rec, httptest.NewRequest(http.MethodGet, "/api/docs", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "SwaggerUIBundle") {
		t.Fatalf("ui handler: code=%d", rec.Code)
	}
}
