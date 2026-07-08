// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/service"
	"github.com/tviviano/ts-store/pkg/store"
)

// respondStoreError writes the HTTP status matching a store-layer error:
// 404 when the store doesn't exist, 409 when a delete is refused because
// dependents link to the store, 500 otherwise. Every handler path whose
// error can originate in store.Open / service.Delete should route
// through here instead of writing a blanket 500 (issue #39).
func respondStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrStoreNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrHasDependents):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
