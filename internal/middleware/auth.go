// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package middleware contains HTTP middleware for the API server.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
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

		// Get API key from header or query param
		apiKeyValue := c.GetHeader("X-API-Key")
		if apiKeyValue == "" {
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

// CORS creates CORS middleware.
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}

		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, X-API-Key, Authorization")
		c.Header("Access-Control-Allow-Credentials", "true")
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
