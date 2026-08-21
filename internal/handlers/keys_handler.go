// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/middleware"
)

// KeysHandler exposes key management over HTTP (issue #176). Registered only
// when server.enable_key_api is true — minting returns the new key's
// plaintext, so a deployment opts in deliberately rather than by default.
type KeysHandler struct {
	keyManager *apikey.Manager
}

// NewKeysHandler creates a new key-management handler.
func NewKeysHandler(keyManager *apikey.Manager) *KeysHandler {
	return &KeysHandler{keyManager: keyManager}
}

// CreateKeyRequest is the wire shape for POST /api/keys. Grants use the same
// syntax as the CLI's --grant flag ("read,write:sensors-*"), so operators
// don't learn two spellings of the same thing.
type CreateKeyRequest struct {
	Grants []string `json:"grants" binding:"required"`
	Note   string   `json:"note,omitempty"`
}

// CreateKeyResponse returns the minted key. The plaintext appears here and
// nowhere else, ever — the registry stores only a hash.
type CreateKeyResponse struct {
	ID     string   `json:"id"`
	APIKey string   `json:"api_key"`
	Grants []string `json:"grants"`
	Note   string   `json:"note,omitempty"`
}

// KeyInfo describes a key without its value.
type KeyInfo struct {
	ID        string   `json:"id"`
	Grants    []string `json:"grants"`
	Note      string   `json:"note,omitempty"`
	CreatedAt string   `json:"created_at"`
	Bootstrap bool     `json:"bootstrap,omitempty"`
}

// authenticatedKey resolves the caller's key, or writes a 401 and returns
// nil. Key management authenticates with an ordinary API key — there is no
// separate credential tier, because the bootstrap key IS an ordinary key
// that happens to hold every grant.
func (h *KeysHandler) authenticatedKey(c *gin.Context) *apikey.RegistryKey {
	provided := c.GetHeader("X-API-Key")
	if provided == "" {
		if authHeader := c.GetHeader("Authorization"); len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			provided = authHeader[7:]
		}
	}
	if provided == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
		return nil
	}
	key, err := h.keyManager.Resolve(provided)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
		return nil
	}
	c.Set(middleware.AuthPassedKey, true)
	return key
}

// Create handles POST /api/keys — mint a key with the requested grants.
//
// The caller may issue no more authority than it already holds: every
// requested grant is checked with apikey.PermitsGrant, which compares store
// PATTERNS as well as access classes. Without that, a key holding
// admin:sensors-* could mint itself read,write,manage:* and the whole point
// of scoped grants would evaporate. The bootstrap key holds everything, so
// it can mint anything — that is what makes it the bootstrap key.
func (h *KeysHandler) Create(c *gin.Context) {
	caller := h.authenticatedKey(c)
	if caller == nil {
		return
	}

	var req CreateKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Grants) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one grant is required"})
		return
	}

	grants := make([]apikey.Grant, 0, len(req.Grants))
	for _, raw := range req.Grants {
		g, err := apikey.ParseGrant(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !apikey.PermitsGrant(caller.Grants, g) {
			// Name the grant, not the caller's own authority: echoing back
			// what the caller holds turns this endpoint into a probe for
			// mapping a key's permissions.
			c.JSON(http.StatusForbidden, gin.H{
				"error": "cannot issue a key with grant " + g.String() + ": exceeds the authority of the key making the request",
			})
			return
		}
		grants = append(grants, g)
	}

	note := req.Note
	if note == "" {
		note = "Minted via API"
	}

	plaintext, entry, err := h.keyManager.Create(note, grants)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, CreateKeyResponse{
		ID:     entry.ID,
		APIKey: plaintext,
		Grants: grantStrings(grants),
		Note:   entry.Note,
	})
}

// List handles GET /api/keys — every key's id, grants, and note. Never a key
// value: the registry holds hashes, so they are not recoverable even here.
//
// The full list is returned rather than only keys the caller could issue.
// Knowing which keys exist is what makes revocation usable, and the response
// discloses no credential.
func (h *KeysHandler) List(c *gin.Context) {
	caller := h.authenticatedKey(c)
	if caller == nil {
		return
	}

	entries, err := h.keyManager.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out := make([]KeyInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, KeyInfo{
			ID:        e.ID,
			Grants:    grantStrings(e.Grants),
			Note:      e.Note,
			CreatedAt: e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			Bootstrap: e.Bootstrap,
		})
	}
	c.JSON(http.StatusOK, gin.H{"keys": out})
}

// Delete handles DELETE /api/keys/:id — revoke a key.
//
// Revoking requires the caller to hold at least the authority the target key
// holds, by the same PermitsGrant check minting uses. Otherwise a narrowly
// scoped key could revoke every other key on the server: destructive, and a
// denial-of-service path that needs no privilege at all.
//
// The bootstrap key can never be revoked here. It is the server's root of
// trust, and losing it over HTTP could lock an operator out of their own
// deployment with no recovery short of shell access. The CLI can do it, with
// a confirmation flag.
func (h *KeysHandler) Delete(c *gin.Context) {
	caller := h.authenticatedKey(c)
	if caller == nil {
		return
	}

	targetID := c.Param("id")
	entries, err := h.keyManager.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var target *apikey.KeyEntry
	for i := range entries {
		if entries[i].ID == targetID {
			target = &entries[i]
			break
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
		return
	}
	if target.Bootstrap {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "the bootstrap key cannot be revoked over the API; use the CLI on the host",
		})
		return
	}

	for _, g := range target.Grants {
		if !apikey.PermitsGrant(caller.Grants, g) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "cannot revoke key " + targetID + ": it holds authority beyond the key making the request",
			})
			return
		}
	}

	if err := h.keyManager.Revoke(targetID); err != nil {
		if errors.Is(err, apikey.ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "key revoked", "id": targetID})
}

func grantStrings(grants []apikey.Grant) []string {
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, g.String())
	}
	return out
}
