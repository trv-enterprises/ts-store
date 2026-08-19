// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package middleware contains HTTP middleware for the API server.
package middleware

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/pkg/store"
)

const (
	// StoreNameKey is the context key for the authenticated store name
	StoreNameKey = "store_name"
	// KeyEntryKey is the context key for the authenticated key entry
	KeyEntryKey = "key_entry"
)

// RouteAccessTable exposes the route→access classification for tests,
// which cross-check it against the router's actual registered routes so
// a new route can't silently inherit the wrong class.
func RouteAccessTable() map[string]apikey.Access { return routeAccess }

// apiKeyFromRequest extracts the caller's API key: X-API-Key, else an
// Authorization: Bearer header.
//
// The api_key query fallback exists only for the browser WebSocket handshake —
// the WebSocket API cannot set request headers. Query credentials land in
// proxy/access logs and browser history, so every other route is header-only.
func apiKeyFromRequest(c *gin.Context) string {
	if v := c.GetHeader("X-API-Key"); v != "" {
		return v
	}
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	if strings.HasSuffix(c.FullPath(), "/ws/write") {
		return c.Query("api_key")
	}
	return ""
}

// Auth creates authentication middleware that validates API keys and
// checks the key's grant for the route's store (issue #138).
//
// access is the group's default class. Individual routes are classified
// in routeAccess below and override it. The class is a property of the
// route, not the request, and is never inferred from the HTTP method:
// method says nothing about authority in either direction — creating a
// push connection is a POST that requires only read, while listing
// rollups is a GET that requires manage.
func Auth(keyManager *apikey.Manager, access apikey.Access) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get store name from URL parameter
		storeName := c.Param("store")
		if storeName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "store name required"})
			c.Abort()
			return
		}

		// Reject traversal-capable names before they reach any code that
		// joins them onto the data path (key lookup reads <name>/keys.json).
		if err := store.ValidateStoreName(storeName); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid store name"})
			c.Abort()
			return
		}

		apiKeyValue := apiKeyFromRequest(c)

		if apiKeyValue == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
			c.Abort()
			return
		}

		// Validate key format
		if !apikey.ValidateKeyFormat(apiKeyValue) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key format"})
			c.Abort()
			return
		}

		// The class is resolved from the matched route rather than the
		// request, so an administrative GET (e.g. listing rollups) still
		// requires "manage" despite looking read-shaped.
		required := accessFor(c.Request.Method, c.FullPath(), access)

		// Resolve the key and check its grant for this store + class.
		keyEntry, err := keyManager.Authorize(storeName, apiKeyValue, required)
		if err != nil {
			switch err {
			case apikey.ErrForbidden:
				// The key is real but not granted here. 403, not 401:
				// retrying with the same credential will never work, and
				// a client that conflates the two will loop.
				c.JSON(http.StatusForbidden, gin.H{
					"error": "API key lacks " + string(required) + " access to store " + storeName,
				})
			case apikey.ErrInvalidKey:
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			default:
				c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication failed"})
			}
			c.Abort()
			return
		}

		// Store authenticated info in context
		c.Set(StoreNameKey, storeName)
		c.Set(KeyEntryKey, keyEntry)
		c.Set(AuthPassedKey, true)

		c.Next()
	}
}

// routeAccess maps "METHOD /full/route/pattern" to the access class that
// route requires, for every route whose class differs from its group's
// default.
//
// This is a table rather than per-route middleware because gin runs
// group middleware before route handlers: a RequireAccess registered on
// the route would execute after Auth and never be seen. Keying off
// c.FullPath() (the matched route pattern, not the request path) keeps
// the lookup exact and free of parameter-substitution guesswork.
//
// The default for the store group is AccessWrite, so anything absent
// here requires write. That default is deliberate: forgetting to
// classify a new route fails closed for read-only keys rather than
// silently exposing it.
var routeAccess = map[string]apikey.Access{
	// --- read: data queries, schema inspection, and stream-out ---
	"GET /api/stores/:store/data/time/:timestamp": apikey.AccessRead,
	"GET /api/stores/:store/data/oldest":          apikey.AccessRead,
	"GET /api/stores/:store/data/newest":          apikey.AccessRead,
	"GET /api/stores/:store/data/range":           apikey.AccessRead,
	"GET /api/stores/:store/schema":               apikey.AccessRead,
	"GET /api/stores/:store/schema/versions":      apikey.AccessRead,
	// Push-connection lifecycle is read, not manage (issue #154): a push
	// connection only ever delivers data the key could already poll, so
	// consumers manage their own subscriptions. Classifying it manage both
	// over-privileged consumers (manage includes reset/schema mutation)
	// and let a manage-only key exfiltrate data it cannot read.
	"GET /api/stores/:store/connections": apikey.AccessRead,
	// Alert READS are observability, not administration (issue #162):
	// which rules exist and whether they are keeping up (fired counts,
	// lag, drop counters) is a question about the store's data. Requiring
	// manage for a dashboard would also hand it alert/rollup CRUD, schema
	// mutation, reset and delete. Safe at this tier only because the
	// detail payload redacts every credential surface — see
	// GetAlertDetail: URLs lose userinfo and query, MQTT passwords are
	// masked, and header values are masked by allowlist (#163). That
	// redaction is a security boundary now, not politeness. Alert
	// MUTATION stays manage, below.
	"GET /api/stores/:store/alerts":                  apikey.AccessRead,
	"GET /api/stores/:store/alerts/:id":              apikey.AccessRead,
	"GET /api/stores/:store/ws/connections":          apikey.AccessRead,
	"POST /api/stores/:store/ws/connections":         apikey.AccessRead,
	"GET /api/stores/:store/ws/connections/:id":      apikey.AccessRead,
	"DELETE /api/stores/:store/ws/connections/:id":   apikey.AccessRead,
	"GET /api/stores/:store/mqtt/connections":        apikey.AccessRead,
	"POST /api/stores/:store/mqtt/connections":       apikey.AccessRead,
	"GET /api/stores/:store/mqtt/connections/:id":    apikey.AccessRead,
	"DELETE /api/stores/:store/mqtt/connections/:id": apikey.AccessRead,

	// --- write: ingest ---
	"POST /api/stores/:store/data":    apikey.AccessWrite,
	"GET /api/stores/:store/ws/write": apikey.AccessWrite,

	// --- manage: store-scoped administration ---
	// Alerts belong to the store, so alert mutation is a per-store grant
	// rather than a server-admin operation. Reads are classified read
	// (see above).
	"POST /api/stores/:store/alerts":        apikey.AccessManage,
	"POST /api/stores/:store/alerts/test":   apikey.AccessManage,
	"PUT /api/stores/:store/alerts/:id":     apikey.AccessManage,
	"DELETE /api/stores/:store/alerts/:id":  apikey.AccessManage,
	"GET /api/stores/:store/rollups":        apikey.AccessManage,
	"POST /api/stores/:store/rollups":       apikey.AccessManage,
	"GET /api/stores/:store/rollups/:id":    apikey.AccessManage,
	"DELETE /api/stores/:store/rollups/:id": apikey.AccessManage,
	// Schema mutation, reset, and store deletion are destructive.
	"PUT /api/stores/:store/schema":         apikey.AccessManage,
	"POST /api/stores/:store/reset":         apikey.AccessManage,
	"POST /api/stores/:store/metrics/reset": apikey.AccessManage,
	"DELETE /api/stores/:store":             apikey.AccessManage,
}

// accessFor resolves the class for a matched route, falling back to the
// group default when the route is not classified.
func accessFor(method, fullPath string, fallback apikey.Access) apikey.Access {
	if a, ok := routeAccess[method+" "+fullPath]; ok {
		return a
	}
	return fallback
}

// GetStoreName retrieves the authenticated store name from context.
func GetStoreName(c *gin.Context) string {
	if v, ok := c.Get(StoreNameKey); ok {
		return v.(string)
	}
	return ""
}

// GetKeyEntry retrieves the authenticated key from context — the
// *RegistryKey that Auth stored after Authorize. Returns nil when absent
// or of an unexpected type (callers must fail closed). The historical
// *KeyEntry assertion here was wrong and panicked on first real use
// (issue #160); Authorize has always returned *RegistryKey.
func GetKeyEntry(c *gin.Context) *apikey.RegistryKey {
	if v, ok := c.Get(KeyEntryKey); ok {
		if key, ok := v.(*apikey.RegistryKey); ok {
			return key
		}
	}
	return nil
}

// storeCreateBody is the subset of the create request the auth layer needs:
// which store name is being created. Decoded from a REPLAYED copy of the
// body so the handler still sees an intact stream.
type storeCreateBody struct {
	Name string `json:"name"`
}

// peekCreateStoreName reads the requested store name out of the JSON body
// without consuming it. Gin's request body is a single-use stream, so it is
// buffered and reinstalled — forgetting that gives the handler an empty body
// and a baffling "name is required".
//
// Returns "" when the body is absent or unparseable; the caller treats that
// as "no name to authorize" and refuses, letting the handler produce the real
// validation error for a genuinely malformed request.
func peekCreateStoreName(c *gin.Context) string {
	if c.Request.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxCreateBodyPeek))
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	var body storeCreateBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return body.Name
}

// maxCreateBodyPeek bounds the buffered copy. A create body is a handful of
// fields; anything larger is not a create request this middleware needs to
// understand.
const maxCreateBodyPeek = 64 << 10

// AdminAuth creates middleware authorizing store creation. Two credentials
// are accepted:
//
//   - X-Admin-Key — the server-tier bootstrap credential, unchanged. A fresh
//     server has an empty key registry, so some credential must exist before
//     any grant can.
//   - X-API-Key — a registry key holding an "admin" grant whose pattern
//     covers the store name being created (issue #157).
//
// The second is why this middleware reads the body. Every other authenticated
// route is /api/stores/:store/..., so the store name comes from the URL and
// Auth checks the grant against it. Creation posts to /api/stores with the
// name in the JSON body, so a pattern-scoped grant like admin:sensors-* can
// only be evaluated by looking there. That is the entire reason for the peek.
func AdminAuth(adminKey string, keyManager *apikey.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		providedKey := c.GetHeader("X-Admin-Key")

		// No admin key, but an API key: try the admin-grant path before
		// falling through to the old "wrong header" error.
		if providedKey == "" && keyManager != nil {
			if apiKeyValue := apiKeyFromRequest(c); apiKeyValue != "" {
				storeName := peekCreateStoreName(c)
				if storeName == "" {
					c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
					c.Abort()
					return
				}
				if err := store.ValidateStoreName(storeName); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid store name"})
					c.Abort()
					return
				}
				key, err := keyManager.Authorize(storeName, apiKeyValue, apikey.AccessAdmin)
				if err != nil {
					switch err {
					case apikey.ErrForbidden:
						c.JSON(http.StatusForbidden, gin.H{
							"error": "API key lacks admin access to create store " + storeName,
						})
					default:
						c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
					}
					c.Abort()
					return
				}
				c.Set(KeyEntryKey, key)
				c.Set(AuthPassedKey, true)
				c.Next()
				return
			}
		}

		if providedKey == "" {
			// Check if they sent X-API-Key instead, and give a helpful hint
			if c.GetHeader("X-API-Key") != "" {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "store creation requires an X-Admin-Key header, or an X-API-Key whose grant includes admin access to that store name"})
			} else {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "admin key required (provide via X-Admin-Key header)"})
			}
			c.Abort()
			return
		}

		// Constant-time comparison to prevent timing attacks
		if !secureCompare(providedKey, adminKey) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid admin key"})
			c.Abort()
			return
		}

		c.Set(AuthPassedKey, true)
		c.Next()
	}
}

// secureCompare performs a constant-time string comparison to prevent
// timing attacks. crypto/subtle does the comparison; the length check is
// unavoidable (ConstantTimeCompare requires equal lengths) but a length
// oracle on a random ≥20-char admin key is not exploitable.
func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// CORS creates CORS middleware. The API is deliberately open to any origin:
// auth is header-based (X-API-Key / X-Admin-Key), never cookies, so a plain
// wildcard carries no ambient credentials. Reflecting the request Origin
// together with Access-Control-Allow-Credentials (the previous behavior) is
// the canonical CORS misconfiguration and would matter the moment any
// cookie- or session-based auth appeared.
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, X-API-Key, X-Admin-Key, Authorization")
		c.Header("Access-Control-Max-Age", "86400")

		// Security headers on every API response: the API serves only
		// JSON/binary it generated (never user-uploaded pages), but
		// nosniff is free insurance against a response being interpreted
		// as HTML, and no-store keeps data and error bodies out of
		// shared/proxy caches — responses here are authenticated,
		// per-request, and never cacheable.
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// RequestLogger creates request logging middleware. Requests that complete
// with status >= 400 are logged with method, path, status, latency and
// client IP, so failed auth and client/server errors are visible in the
// server log (probing, misconfigured clients). Successful requests are not
// logged — they'd swamp the log at sensor write rates.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip logging for health checks
		if strings.HasPrefix(c.Request.URL.Path, "/health") {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		if status >= 400 {
			log.Printf("http: %s %s -> %d (%s) from %s",
				c.Request.Method, redactedRequestURL(c.Request.URL), status,
				time.Since(start).Round(time.Millisecond), c.ClientIP())
		}
	}
}

// redactedRequestURL renders path?query with credential-bearing query
// values (api_key, admin_key) masked, so keys sent via query string never
// land in the log.
func redactedRequestURL(u *url.URL) string {
	if u.RawQuery == "" {
		return u.Path
	}
	q := u.Query()
	for _, k := range []string{"api_key", "admin_key"} {
		if q.Has(k) {
			q.Set(k, "REDACTED")
		}
	}
	return u.Path + "?" + q.Encode()
}
