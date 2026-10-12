package magento

import (
	"strings"
	"testing"
)

func TestGenerateDefaultVCLBackend(t *testing.T) {
	body := GenerateDefaultVCL("store", false)
	want := `.host = "store-frontend";`
	if !strings.Contains(body, want) {
		t.Errorf("backend host missing: want substring %q in body:\n%s", want, body)
	}
	if !strings.Contains(body, `.port = "8080"`) {
		t.Errorf("backend port 8080 missing in body:\n%s", body)
	}
}

func TestGenerateDefaultVCLBypasses(t *testing.T) {
	body := GenerateDefaultVCL("store", false)
	for _, path := range []string{`"^/admin"`, `"^/rest"`, `"^/graphql"`} {
		if !strings.Contains(body, path) {
			t.Errorf("missing bypass for %s in body:\n%s", path, body)
		}
	}
	if !strings.Contains(body, `req.method != "GET" && req.method != "HEAD"`) {
		t.Errorf("missing non-GET/HEAD bypass in body:\n%s", body)
	}
}

func TestGenerateDefaultVCLMultiStoreToggle(t *testing.T) {
	single := GenerateDefaultVCL("store", false)
	multi := GenerateDefaultVCL("store", true)

	hashLine := "hash_data(req.http.host)"
	if strings.Contains(single, hashLine) {
		t.Errorf("single-store body should not contain %q (Varnish 4.1 default vcl_hash applies):\n%s", hashLine, single)
	}
	if !strings.Contains(multi, hashLine) {
		t.Errorf("multi-store body must contain %q per ADR 007:\n%s", hashLine, multi)
	}
	if !strings.Contains(multi, "sub vcl_hash") {
		t.Errorf("multi-store body must define sub vcl_hash:\n%s", multi)
	}
}

func TestGenerateDefaultVCLBackendUsesStoreName(t *testing.T) {
	body := GenerateDefaultVCL("acme", false)
	want := `.host = "acme-frontend";`
	if !strings.Contains(body, want) {
		t.Errorf("backend host should follow <storeName>-frontend convention: want %q in body:\n%s", want, body)
	}
}

func TestGenerateDefaultVCLDoesNotCache5xx(t *testing.T) {
	body := GenerateDefaultVCL("store", false)
	if !strings.Contains(body, "if (beresp.status >= 500)") {
		t.Errorf("5xx bypass missing: want substring \"if (beresp.status >= 500)\" in body:\n%s", body)
	}
	if !strings.Contains(body, "set beresp.uncacheable = true") {
		t.Errorf("5xx uncacheable directive missing in body:\n%s", body)
	}
	if !strings.Contains(body, "set beresp.ttl = 0s") {
		t.Errorf("5xx ttl reset missing in body:\n%s", body)
	}
}
