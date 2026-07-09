// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package middleware contains HTTP middleware for the API server.
package middleware

import (
	"net/http"
	"strings"

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

// Auth creates authentication middleware that validates API keys.
// The store name is extracted from the URL path parameter.
func Auth(keyManager *apikey.Manager) gin.HandlerFunc {
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

		// Get API key from header: X-API-Key or Authorization: Bearer.
		apiKeyValue := c.GetHeader("X-API-Key")
		if apiKeyValue == "" {
			// Check Authorization: Bearer header
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				apiKeyValue = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}
		// The api_key query fallback exists only for the browser WebSocket
		// handshake — the WebSocket API can't set request headers. Query
		// credentials land in proxy/access logs and browser history, so
		// every other route is header-only.
		if apiKeyValue == "" && strings.HasSuffix(c.FullPath(), "/ws/write") {
			apiKeyValue = c.Query("api_key")
		}

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

		// Validate key against store
		keyEntry, err := keyManager.Validate(storeName, apiKeyValue)
		if err != nil {
			if err == apikey.ErrInvalidKey {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			} else {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication failed"})
			}
			c.Abort()
			return
		}

		// Store authenticated info in context
		c.Set(StoreNameKey, storeName)
		c.Set(KeyEntryKey, keyEntry)

		c.Next()
	}
}

// GetStoreName retrieves the authenticated store name from context.
func GetStoreName(c *gin.Context) string {
	if v, ok := c.Get(StoreNameKey); ok {
		return v.(string)
	}
	return ""
}

// GetKeyEntry retrieves the authenticated key entry from context.
func GetKeyEntry(c *gin.Context) *apikey.KeyEntry {
	if v, ok := c.Get(KeyEntryKey); ok {
		return v.(*apikey.KeyEntry)
	}
	return nil
}

// AdminAuth creates middleware that validates the admin key for store
// management operations. The admin key must be provided via the
// X-Admin-Key header — the former admin_key query fallback is gone (query
// credentials leak into proxy/access logs, and nothing header-incapable
// ever needs admin operations).
func AdminAuth(adminKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		providedKey := c.GetHeader("X-Admin-Key")

		if providedKey == "" {
			// Check if they sent X-API-Key instead, and give a helpful hint
			if c.GetHeader("X-API-Key") != "" {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "store creation requires X-Admin-Key header, not X-API-Key"})
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

		c.Next()
	}
}

// secureCompare performs a constant-time string comparison to prevent timing attacks.
func secureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}
	return result == 0
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

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// RequestLogger creates request logging middleware.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip logging for health checks
		if strings.HasPrefix(c.Request.URL.Path, "/health") {
			c.Next()
			return
		}

		c.Next()

		// Log after request completes
		status := c.Writer.Status()
		if status >= 400 {
			// Log errors - could integrate with proper logger
			_ = status // placeholder
		}
	}
}
