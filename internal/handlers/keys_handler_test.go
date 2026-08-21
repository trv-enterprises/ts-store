// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
)

// setupKeysRouter wires the key API the way main.go does when
// server.enable_key_api is true, and returns the bootstrap key.
func setupKeysRouter(t *testing.T) (*gin.Engine, *apikey.Manager, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	km := apikey.NewManager(t.TempDir())
	boot, err := km.EnsureBootstrap("")
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	h := NewKeysHandler(km)
	r := gin.New()
	keys := r.Group("/api/keys")
	keys.POST("", h.Create)
	keys.GET("", h.List)
	keys.DELETE("/:id", h.Delete)

	return r, km, boot.Plaintext
}

func mintKey(t *testing.T, r *gin.Engine, callerKey string, grants []string, note string) (int, CreateKeyResponse, string) {
	t.Helper()
	body, _ := json.Marshal(CreateKeyRequest{Grants: grants, Note: note})
	req, _ := http.NewRequest("POST", "/api/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if callerKey != "" {
		req.Header.Set("X-API-Key", callerKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp CreateKeyResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp, w.Body.String()
}

// TestKeyAPIMintAndUse: the bootstrap key mints a scoped key, and the minted
// key actually works with exactly the authority requested.
func TestKeyAPIMintAndUse(t *testing.T) {
	r, km, bootstrapKey := setupKeysRouter(t)

	code, resp, body := mintKey(t, r, bootstrapKey, []string{"read,write:sensors-*"}, "collector")
	if code != http.StatusCreated {
		t.Fatalf("mint: got %d (%s), want 201", code, body)
	}
	if !apikey.ValidateKeyFormat(resp.APIKey) {
		t.Errorf("minted key has the wrong shape: %q", resp.APIKey)
	}
	if resp.ID == "" {
		t.Error("response carries no key id")
	}

	key, err := km.Resolve(resp.APIKey)
	if err != nil {
		t.Fatalf("minted key does not resolve: %v", err)
	}
	if !key.Permits("sensors-garage", apikey.AccessRead) || !key.Permits("sensors-garage", apikey.AccessWrite) {
		t.Error("minted key lacks the grants it was created with")
	}
	if key.Permits("billing", apikey.AccessRead) {
		t.Error("minted key reaches beyond its pattern")
	}
	if key.Permits("sensors-garage", apikey.AccessManage) {
		t.Error("minted key holds a class it was not granted")
	}
}

// TestKeyAPIRefusesEscalation is the security property: a key may issue no
// more authority than it holds. Each case here is a privilege escalation that
// must be refused (issue #176).
func TestKeyAPIRefusesEscalation(t *testing.T) {
	r, _, bootstrapKey := setupKeysRouter(t)

	// A narrowly-scoped provisioner minted by the bootstrap key.
	code, provisioner, body := mintKey(t, r, bootstrapKey, []string{"admin:sensors-*"}, "provisioner")
	if code != http.StatusCreated {
		t.Fatalf("mint provisioner: got %d (%s)", code, body)
	}

	escalations := []struct {
		name   string
		grants []string
	}{
		{"admin-only key granting itself data access", []string{"read:sensors-garage"}},
		{"narrower pattern widening to everything", []string{"admin:*"}},
		{"narrower pattern widening to a sibling glob", []string{"admin:sens-*"}},
		{"reaching a store outside the pattern", []string{"admin:billing"}},
		{"one legal grant smuggling an illegal one", []string{"admin:sensors-x", "manage:sensors-x"}},
	}
	for _, e := range escalations {
		if code, _, body := mintKey(t, r, provisioner.APIKey, e.grants, ""); code != http.StatusForbidden {
			t.Errorf("%s: got %d (%s), want 403", e.name, code, body)
		}
	}

	// What it legitimately may do still works.
	if code, _, body := mintKey(t, r, provisioner.APIKey, []string{"admin:sensors-garage"}, ""); code != http.StatusCreated {
		t.Errorf("legitimate narrowing mint: got %d (%s), want 201", code, body)
	}
}

// TestKeyAPIErrorDoesNotDiscloseCallerAuthority: the 403 says what was
// refused, not what the caller holds — otherwise the endpoint becomes a probe
// for mapping a key's permissions.
func TestKeyAPIErrorDoesNotDiscloseCallerAuthority(t *testing.T) {
	r, _, bootstrapKey := setupKeysRouter(t)

	_, provisioner, _ := mintKey(t, r, bootstrapKey, []string{"admin:sensors-*"}, "provisioner")
	_, _, body := mintKey(t, r, provisioner.APIKey, []string{"read:billing"}, "")

	if bytes.Contains([]byte(body), []byte("sensors-*")) {
		t.Errorf("403 body discloses the caller's own grants: %s", body)
	}
}

// TestKeyAPIListNeverReturnsKeyValues: listing is how revocation becomes
// usable, but the registry stores hashes and no endpoint may surface a key.
func TestKeyAPIListNeverReturnsKeyValues(t *testing.T) {
	r, _, bootstrapKey := setupKeysRouter(t)

	_, minted, _ := mintKey(t, r, bootstrapKey, []string{"read:*"}, "dashboard")

	req, _ := http.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("X-API-Key", bootstrapKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d (%s), want 200", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{minted.APIKey, bootstrapKey} {
		if bytes.Contains([]byte(body), []byte(secret)) {
			t.Error("a key VALUE appeared in the listing")
		}
	}

	var resp struct {
		Keys []KeyInfo `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	var sawBootstrap bool
	for _, k := range resp.Keys {
		if k.Bootstrap {
			sawBootstrap = true
		}
	}
	if !sawBootstrap {
		t.Error("listing does not mark the bootstrap key, so callers cannot tell it apart")
	}
}

// TestKeyAPIRevoke: a key can be revoked and immediately stops working.
func TestKeyAPIRevoke(t *testing.T) {
	r, km, bootstrapKey := setupKeysRouter(t)

	_, minted, _ := mintKey(t, r, bootstrapKey, []string{"read:*"}, "temporary")
	if _, err := km.Resolve(minted.APIKey); err != nil {
		t.Fatalf("minted key does not resolve: %v", err)
	}

	req, _ := http.NewRequest("DELETE", "/api/keys/"+minted.ID, nil)
	req.Header.Set("X-API-Key", bootstrapKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: got %d (%s), want 200", w.Code, w.Body.String())
	}

	if _, err := km.Resolve(minted.APIKey); err == nil {
		t.Error("revoked key still resolves")
	}
}

// TestKeyAPICannotRevokeBootstrapKey: losing the root of trust over HTTP
// could lock an operator out of their own deployment with no recovery short
// of shell access, so the API refuses regardless of the caller's authority.
func TestKeyAPICannotRevokeBootstrapKey(t *testing.T) {
	r, km, bootstrapKey := setupKeysRouter(t)

	entries, err := km.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var bootID string
	for _, e := range entries {
		if e.Bootstrap {
			bootID = e.ID
		}
	}
	if bootID == "" {
		t.Fatal("no bootstrap key in the registry")
	}

	req, _ := http.NewRequest("DELETE", "/api/keys/"+bootID, nil)
	req.Header.Set("X-API-Key", bootstrapKey) // the most privileged caller there is
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("revoking the bootstrap key: got %d, want 403", w.Code)
	}
	if _, err := km.Resolve(bootstrapKey); err != nil {
		t.Error("the bootstrap key stopped working after a refused revoke")
	}
}

// TestKeyAPIRevokeRequiresSufficientAuthority: without this, any key could
// revoke every other key — destructive, and a denial-of-service path needing
// no privilege at all.
func TestKeyAPIRevokeRequiresSufficientAuthority(t *testing.T) {
	r, _, bootstrapKey := setupKeysRouter(t)

	_, powerful, _ := mintKey(t, r, bootstrapKey, []string{"read,write,manage:*"}, "powerful")
	_, weak, _ := mintKey(t, r, bootstrapKey, []string{"read:sensors-*"}, "weak")

	req, _ := http.NewRequest("DELETE", "/api/keys/"+powerful.ID, nil)
	req.Header.Set("X-API-Key", weak.APIKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("weak key revoking a stronger one: got %d, want 403", w.Code)
	}

	// The reverse direction is allowed.
	req, _ = http.NewRequest("DELETE", "/api/keys/"+weak.ID, nil)
	req.Header.Set("X-API-Key", powerful.APIKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("stronger key revoking a weaker one: got %d (%s), want 200", w.Code, w.Body.String())
	}
}

// TestKeyAPIRequiresAuthentication: no anonymous access to any verb.
func TestKeyAPIRequiresAuthentication(t *testing.T) {
	r, _, _ := setupKeysRouter(t)

	if code, _, _ := mintKey(t, r, "", []string{"read:*"}, ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous mint: got %d, want 401", code)
	}
	for _, m := range []struct{ method, path string }{
		{"GET", "/api/keys"},
		{"DELETE", "/api/keys/abc12345"},
	} {
		req, _ := http.NewRequest(m.method, m.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: got %d, want 401", m.method, m.path, w.Code)
		}
	}
}
