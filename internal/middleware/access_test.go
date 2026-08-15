// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
)

// The access classes are the whole point of #138, so these tests check
// the classification itself — not just that authorization runs.

// TestAccessForFallsBackToGroupDefault: an unclassified route must
// inherit the group default rather than silently becoming read.
func TestAccessForFallsBackToGroupDefault(t *testing.T) {
	if got := accessFor("GET", "/api/stores/:store/not-a-real-route", apikey.AccessWrite); got != apikey.AccessWrite {
		t.Errorf("unclassified route resolved to %q, want the group default %q", got, apikey.AccessWrite)
	}
	// The fallback is write, not read: a new endpoint added without a
	// table entry must fail closed for a read-only key.
	if got := accessFor("POST", "/api/stores/:store/brand-new", apikey.AccessWrite); got == apikey.AccessRead {
		t.Error("unclassified route defaulted to read; new endpoints must fail closed")
	}
}

// TestReadRoutesAreClassifiedRead pins the data-read endpoints, which
// are the ones a dashboard key must reach.
func TestReadRoutesAreClassifiedRead(t *testing.T) {
	readRoutes := []string{
		"GET /api/stores/:store/data/range",
		"GET /api/stores/:store/data/newest",
		"GET /api/stores/:store/data/oldest",
		"GET /api/stores/:store/data/time/:timestamp",
		"GET /api/stores/:store/schema",
		// Push-connection lifecycle is stream-out, which read covers
		// (issue #154): a consumer key must be able to manage its own
		// subscriptions without holding manage's reset/schema powers,
		// and a manage-only key must not gain data access via a push
		// connection it points at itself.
		"GET /api/stores/:store/connections",
		"POST /api/stores/:store/ws/connections",
		"DELETE /api/stores/:store/ws/connections/:id",
		"POST /api/stores/:store/mqtt/connections",
		"DELETE /api/stores/:store/mqtt/connections/:id",
		// Alert reads are observability, not administration (issue #162).
		// Safe at this tier because the detail payload redacts every
		// credential surface (#163).
		"GET /api/stores/:store/alerts",
		"GET /api/stores/:store/alerts/:id",
	}
	for _, r := range readRoutes {
		method, path, _ := strings.Cut(r, " ")
		if got := accessFor(method, path, apikey.AccessWrite); got != apikey.AccessRead {
			t.Errorf("%s classified %q, want read", r, got)
		}
	}
}

// TestAdminShapedRoutesRequireManage is the classification that matters
// most: several are GETs, so inferring the class from the HTTP method
// would have handed them to read-only keys.
func TestAdminShapedRoutesRequireManage(t *testing.T) {
	manageRoutes := []string{
		"POST /api/stores/:store/alerts",
		"POST /api/stores/:store/alerts/test",
		"DELETE /api/stores/:store/alerts/:id",
		"GET /api/stores/:store/rollups",
		"PUT /api/stores/:store/schema",
		"POST /api/stores/:store/reset",
		"DELETE /api/stores/:store",
	}
	for _, r := range manageRoutes {
		method, path, _ := strings.Cut(r, " ")
		if got := accessFor(method, path, apikey.AccessWrite); got != apikey.AccessManage {
			t.Errorf("%s classified %q, want manage", r, got)
		}
	}
}

// TestWriteRoutesAreClassifiedWrite: ingest must not be reachable by a
// read-only key, and must not require manage either (collectors hold
// write-only keys).
func TestWriteRoutesAreClassifiedWrite(t *testing.T) {
	for _, r := range []string{"POST /api/stores/:store/data", "GET /api/stores/:store/ws/write"} {
		method, path, _ := strings.Cut(r, " ")
		if got := accessFor(method, path, apikey.AccessWrite); got != apikey.AccessWrite {
			t.Errorf("%s classified %q, want write", r, got)
		}
	}
}

// TestRouteAccessTableIsWellFormed guards against typos in the table
// keys, which would silently never match and leave a route on the
// group default.
func TestRouteAccessTableIsWellFormed(t *testing.T) {
	validMethods := map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true}
	for route, access := range RouteAccessTable() {
		method, path, found := strings.Cut(route, " ")
		if !found {
			t.Errorf("route key %q is not \"METHOD /path\"", route)
			continue
		}
		if !validMethods[method] {
			t.Errorf("route key %q has an unrecognized method %q", route, method)
		}
		if !strings.HasPrefix(path, "/api/stores/:store") {
			t.Errorf("route key %q does not look like a store-scoped route", route)
		}
		if _, err := apikey.ParseAccess(string(access)); err != nil {
			t.Errorf("route %q has invalid access %q: %v", route, access, err)
		}
	}
}

// storeScopedRoutes is the set of authenticated store-scoped routes the
// server registers, captured from gin's route table (TSSTORE_MODE=debug
// prints them). Kept here so the classification table can be diffed
// against reality: a table key that matches no real route silently
// leaves that route on the group default, which is the failure mode a
// typo produces.
//
// If a route is added to cmd/tsstore/main.go, add it here too — the test
// below then forces a deliberate decision about its access class.
var storeScopedRoutes = []string{
	"DELETE /api/stores/:store",
	"DELETE /api/stores/:store/alerts/:id",
	"DELETE /api/stores/:store/mqtt/connections/:id",
	"DELETE /api/stores/:store/rollups/:id",
	"DELETE /api/stores/:store/ws/connections/:id",
	"GET /api/stores/:store/alerts",
	"GET /api/stores/:store/alerts/:id",
	"GET /api/stores/:store/connections",
	"GET /api/stores/:store/data/newest",
	"GET /api/stores/:store/data/oldest",
	"GET /api/stores/:store/data/range",
	"GET /api/stores/:store/data/time/:timestamp",
	"GET /api/stores/:store/mqtt/connections",
	"GET /api/stores/:store/mqtt/connections/:id",
	"GET /api/stores/:store/rollups",
	"GET /api/stores/:store/rollups/:id",
	"GET /api/stores/:store/schema",
	"GET /api/stores/:store/schema/versions",
	"GET /api/stores/:store/ws/connections",
	"GET /api/stores/:store/ws/connections/:id",
	"GET /api/stores/:store/ws/write",
	"POST /api/stores/:store/alerts",
	"POST /api/stores/:store/alerts/test",
	"POST /api/stores/:store/data",
	"POST /api/stores/:store/metrics/reset",
	"POST /api/stores/:store/mqtt/connections",
	"POST /api/stores/:store/reset",
	"POST /api/stores/:store/rollups",
	"POST /api/stores/:store/ws/connections",
	"PUT /api/stores/:store/schema",
}

// TestEveryStoreRouteIsClassified: every authenticated store route must
// have a deliberate access class. Without this, adding an endpoint and
// forgetting the table entry silently gives it the group default.
func TestEveryStoreRouteIsClassified(t *testing.T) {
	table := RouteAccessTable()
	for _, route := range storeScopedRoutes {
		if _, ok := table[route]; !ok {
			t.Errorf("route %q has no access class; add it to routeAccess", route)
		}
	}
}

// TestNoDeadTableEntries: a key matching no registered route is a typo,
// and typos fail silently (the route just keeps the group default).
func TestNoDeadTableEntries(t *testing.T) {
	registered := make(map[string]bool, len(storeScopedRoutes))
	for _, r := range storeScopedRoutes {
		registered[r] = true
	}
	for route := range RouteAccessTable() {
		if !registered[route] {
			t.Errorf("routeAccess key %q matches no registered route (typo?)", route)
		}
	}
}

// TestAuthEnforcesAccessClass drives the real middleware end to end: a
// read-only key reaches a read route and is refused a manage route with
// 403 (not 401 — the credential is valid, the authority isn't).
func TestAuthEnforcesAccessClass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km := apikey.NewManager(t.TempDir())

	roKey, _, err := km.Create("read-only", []apikey.Grant{{
		Stores: "teststore", Access: []apikey.Access{apikey.AccessRead},
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := gin.New()
	g := router.Group("/api/stores/:store")
	g.Use(Auth(km, apikey.AccessWrite))
	g.GET("/data/range", func(c *gin.Context) { c.Status(http.StatusOK) })
	g.GET("/rollups", func(c *gin.Context) { c.Status(http.StatusOK) })
	g.POST("/data", func(c *gin.Context) { c.Status(http.StatusOK) })

	call := func(method, path string) int {
		req, _ := http.NewRequest(method, path, nil)
		req.Header.Set("X-API-Key", roKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	if code := call("GET", "/api/stores/teststore/data/range"); code != http.StatusOK {
		t.Errorf("read key on a read route: got %d, want 200", code)
	}
	if code := call("GET", "/api/stores/teststore/rollups"); code != http.StatusForbidden {
		t.Errorf("read key on a manage route: got %d, want 403", code)
	}
	if code := call("POST", "/api/stores/teststore/data"); code != http.StatusForbidden {
		t.Errorf("read key on a write route: got %d, want 403", code)
	}
}

// TestAlertReadsSplitFromAlertMutation pins issue #162 end to end: a
// read-only key — the dashboard persona — reaches both alert read
// endpoints but is refused every mutating one, so "watch rule health"
// no longer requires the authority to reconfigure or delete alerts
// (and, via manage, to reset or drop the store).
func TestAlertReadsSplitFromAlertMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km := apikey.NewManager(t.TempDir())

	roKey, _, err := km.Create("dashboard", []apikey.Grant{{
		Stores: "teststore", Access: []apikey.Access{apikey.AccessRead},
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	router := gin.New()
	g := router.Group("/api/stores/:store")
	g.Use(Auth(km, apikey.AccessWrite))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	g.GET("/alerts", ok)
	g.GET("/alerts/:id", ok)
	g.POST("/alerts", ok)
	g.POST("/alerts/test", ok)
	g.DELETE("/alerts/:id", ok)

	call := func(method, path string) int {
		req, _ := http.NewRequest(method, path, nil)
		req.Header.Set("X-API-Key", roKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	for _, r := range []struct{ method, path string }{
		{"GET", "/api/stores/teststore/alerts"},
		{"GET", "/api/stores/teststore/alerts/abc12345"},
	} {
		if code := call(r.method, r.path); code != http.StatusOK {
			t.Errorf("read key on %s %s: got %d, want 200", r.method, r.path, code)
		}
	}

	for _, r := range []struct{ method, path string }{
		{"POST", "/api/stores/teststore/alerts"},
		{"POST", "/api/stores/teststore/alerts/test"},
		{"DELETE", "/api/stores/teststore/alerts/abc12345"},
	} {
		if code := call(r.method, r.path); code != http.StatusForbidden {
			t.Errorf("read key on %s %s: got %d, want 403", r.method, r.path, code)
		}
	}
}

// TestManageOnlyKeyCannotStreamOut pins the issue #154 hole shut: access
// classes are independent (manage does not imply read), and a push
// connection delivers store data — so a manage-only key must be refused
// the connection routes it could otherwise point at itself, while a
// read key (the consumer) must reach them.
func TestManageOnlyKeyCannotStreamOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km := apikey.NewManager(t.TempDir())

	mgmtKey, _, err := km.Create("alerts-admin", []apikey.Grant{{
		Stores: "teststore", Access: []apikey.Access{apikey.AccessManage},
	}})
	if err != nil {
		t.Fatalf("Create manage key: %v", err)
	}
	roKey, _, err := km.Create("consumer", []apikey.Grant{{
		Stores: "teststore", Access: []apikey.Access{apikey.AccessRead},
	}})
	if err != nil {
		t.Fatalf("Create read key: %v", err)
	}

	router := gin.New()
	g := router.Group("/api/stores/:store")
	g.Use(Auth(km, apikey.AccessWrite))
	g.POST("/ws/connections", func(c *gin.Context) { c.Status(http.StatusOK) })

	call := func(key string) int {
		req, _ := http.NewRequest("POST", "/api/stores/teststore/ws/connections", nil)
		req.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	if code := call(mgmtKey); code != http.StatusForbidden {
		t.Errorf("manage-only key on push-connection create: got %d, want 403", code)
	}
	if code := call(roKey); code != http.StatusOK {
		t.Errorf("read key on push-connection create: got %d, want 200", code)
	}
}

// TestAuthRejectsUnknownKeyWith401 keeps the 401/403 split honest:
// an unknown credential is 401, a known one lacking authority is 403.
func TestAuthRejectsUnknownKeyWith401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km := apikey.NewManager(t.TempDir())

	router := gin.New()
	g := router.Group("/api/stores/:store")
	g.Use(Auth(km, apikey.AccessRead))
	g.GET("/data/range", func(c *gin.Context) { c.Status(http.StatusOK) })

	req, _ := http.NewRequest("GET", "/api/stores/teststore/data/range", nil)
	req.Header.Set("X-API-Key", "tsstore_00000000-0000-0000-0000-000000000000")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: got %d, want 401", w.Code)
	}
}

// TestAuthScopesKeyToItsGrantedStore: a key valid on one store must not
// reach another.
func TestAuthScopesKeyToItsGrantedStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km := apikey.NewManager(t.TempDir())

	key, _, err := km.CreateForStore("mine", "test")
	if err != nil {
		t.Fatalf("CreateForStore: %v", err)
	}

	router := gin.New()
	g := router.Group("/api/stores/:store")
	g.Use(Auth(km, apikey.AccessRead))
	g.GET("/data/range", func(c *gin.Context) { c.Status(http.StatusOK) })

	for store, want := range map[string]int{"mine": http.StatusOK, "yours": http.StatusForbidden} {
		req, _ := http.NewRequest("GET", "/api/stores/"+store+"/data/range", nil)
		req.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("key on store %q: got %d, want %d", store, w.Code, want)
		}
	}
}
