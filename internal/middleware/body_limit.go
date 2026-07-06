// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxRequestBodyBytes bounds any request body buffered into memory. Handlers
// read bodies with io.ReadAll / JSON binding, so without a cap a single
// oversized POST can exhaust RAM on a constrained device. 4MB comfortably
// covers the largest legitimate record payloads (objects may span blocks,
// but multi-megabyte single records are outside the design envelope).
const MaxRequestBodyBytes = 4 << 20

// BodyLimit wraps every request body in http.MaxBytesReader. Reads past the
// limit fail with "request body too large" and the connection is closed.
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}
