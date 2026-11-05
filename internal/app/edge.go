package app

import (
	"time"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// Rate limit groups (ADR-0021 §5). An operation joins a group with the
// x-rate-limit extension; without it GET, HEAD and OPTIONS are "read" and
// every other method is "write". auth and sitemap callers have no
// identity.
//
// The values are a proposal and were not measured (no production
// traffic yet). They are derived from Java's per-IP limits
// (RateLimitFilter): per identity mostly Java's value (read 2.5x), per
// address mostly 5x for carrier NAT (auth 3x, sitemap 1x); the bursts are
// new. ADR-0021's table is the single source: TestRateLimitGroups reads it.
const (
	groupAuth     = "auth"
	groupUpload   = "upload"
	groupMessage  = "message"
	groupSitemap  = "sitemap"
	groupBoarding = "boarding"
)

// perMinute is a bucket of burst requests that refills n per minute.
func perMinute(n, burst int) httpx.Rate {
	return httpx.Rate{Every: time.Minute / time.Duration(n), Burst: burst}
}

// rateLimitGroups are the production limits (proposal, not measured);
// TestRateLimitGroups compares them with ADR-0021's table.
func rateLimitGroups() map[string]httpx.GroupLimits {
	return map[string]httpx.GroupLimits{
		httpx.GroupRead:  {PerIP: perMinute(600, 120), PerIdentity: perMinute(300, 60)},
		httpx.GroupWrite: {PerIP: perMinute(300, 60), PerIdentity: perMinute(60, 20)},
		// Login, registration, OTP and password reset. The module adds a
		// KeyCustom bucket per hashed target (Java: path + IP + target).
		groupAuth:     {PerIP: perMinute(30, 10), PerCustom: perMinute(10, 5)},
		groupUpload:   {PerIP: perMinute(100, 20), PerIdentity: perMinute(20, 5)},
		groupMessage:  {PerIP: perMinute(150, 30), PerIdentity: perMinute(30, 10)},
		groupSitemap:  {PerIP: perMinute(30, 10)},
		groupBoarding: {PerIP: perMinute(600, 60), PerIdentity: perMinute(120, 20)},
	}
}
