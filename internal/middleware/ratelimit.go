// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package middleware

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// AuthPassedKey is the context key Auth/AdminAuth set on successful
// authentication. AuthFailureLimiter uses it to reset an IP's failure
// counter only when a request actually authenticated — an unauthenticated
// 200 (e.g. a public endpoint) must not wipe the count, or an attacker
// could interleave probes with public requests to stay under the threshold.
const AuthPassedKey = "auth_passed"

// Default limiter policy: after threshold consecutive 401s from one IP the
// IP is blocked, first for baseBlock, doubling on each subsequent block up
// to maxBlock. Any successful authentication clears the IP's state.
const (
	defaultFailureThreshold = 10
	defaultBaseBlock        = 30 * time.Second
	defaultMaxBlock         = 15 * time.Minute

	// Entries idle this long are dropped; sweeps run at most every sweepEvery.
	entryIdleTTL = time.Hour
	sweepEvery   = 5 * time.Minute
)

type ipFailures struct {
	consecutive  int       // consecutive 401s since last success/block
	blocks       int       // completed blocks, drives exponential backoff
	blockedUntil time.Time // zero when not blocked
	lastSeen     time.Time
}

// AuthLimiter tracks consecutive authentication failures per client IP and
// temporarily blocks IPs that keep failing, so the admin key and per-store
// keys can't be brute-forced at wire speed. In-memory only: state resets on
// restart, which is fine — the point is throttling, not a ban list.
type AuthLimiter struct {
	mu        sync.Mutex
	entries   map[string]*ipFailures
	threshold int
	baseBlock time.Duration
	maxBlock  time.Duration
	lastSweep time.Time
	now       func() time.Time // swapped in tests
}

// NewAuthLimiter creates a limiter. Zero/negative arguments select the
// defaults (10 consecutive failures; 30s block doubling to 15m).
func NewAuthLimiter(threshold int, baseBlock, maxBlock time.Duration) *AuthLimiter {
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}
	if baseBlock <= 0 {
		baseBlock = defaultBaseBlock
	}
	if maxBlock <= 0 {
		maxBlock = defaultMaxBlock
	}
	return &AuthLimiter{
		entries:   make(map[string]*ipFailures),
		threshold: threshold,
		baseBlock: baseBlock,
		maxBlock:  maxBlock,
		now:       time.Now,
	}
}

// BlockedFor returns how much longer the IP is blocked (0 if it isn't).
func (l *AuthLimiter) BlockedFor(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return 0
	}
	if wait := e.blockedUntil.Sub(l.now()); wait > 0 {
		e.lastSeen = l.now()
		return wait
	}
	return 0
}

// RecordFailure counts one auth failure for the IP. When the consecutive
// count reaches the threshold it starts a block and returns its duration;
// otherwise it returns 0.
func (l *AuthLimiter) RecordFailure(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.maybeSweep(now)

	e, ok := l.entries[ip]
	if !ok {
		e = &ipFailures{}
		l.entries[ip] = e
	}
	e.lastSeen = now
	e.consecutive++
	if e.consecutive < l.threshold {
		return 0
	}

	block := l.baseBlock << uint(min(e.blocks, 30))
	if block > l.maxBlock || block <= 0 {
		block = l.maxBlock
	}
	e.blocks++
	e.consecutive = 0
	e.blockedUntil = now.Add(block)
	return block
}

// RecordSuccess clears the IP's failure state after a successful
// authentication.
func (l *AuthLimiter) RecordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// maybeSweep drops long-idle entries so the map stays bounded under
// address churn. Caller holds l.mu.
func (l *AuthLimiter) maybeSweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for ip, e := range l.entries {
		if now.Sub(e.lastSeen) > entryIdleTTL && !e.blockedUntil.After(now) {
			delete(l.entries, ip)
		}
	}
}

// AuthFailureLimiter rate-limits authentication failures per client IP.
// Requests from a blocked IP are rejected with 429 + Retry-After before any
// key comparison runs. Placed in front of Auth/AdminAuth; it watches the
// response status, counting 401s as failures and treating requests that set
// AuthPassedKey as successes.
func AuthFailureLimiter(l *AuthLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if wait := l.BlockedFor(ip); wait > 0 {
			c.Header("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many failed authentication attempts; retry later"})
			c.Abort()
			return
		}

		c.Next()

		if c.Writer.Status() == http.StatusUnauthorized {
			if block := l.RecordFailure(ip); block > 0 {
				log.Printf("auth: blocking %s for %s after repeated authentication failures", ip, block)
			}
		} else if c.GetBool(AuthPassedKey) {
			l.RecordSuccess(ip)
		}
	}
}
