package app

import (
	"time"

	"github.com/is-koala/koala-api/internal/platform/config"
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
	// groupAuthAccount holds the account-wide sign-in buckets, one per
	// target from any address (ADR-0025 §8). No route is in it.
	groupAuthAccount = "auth_account"
	// groupRegistrationAddress holds the per-client-address budget of
	// registration starts (ADR-0032 Karar 6). No route is in it.
	groupRegistrationAddress = "registration_address"
	// defaultRegistrationStartsPerIP is that budget an hour, and its burst;
	// KOALA_REGISTRATION_STARTS_PER_IP_HOUR overrides it.
	defaultRegistrationStartsPerIP = 10
	// groupVerificationAddress holds the per-client-address budget of
	// verification starts (ADR-0033 Karar 4). No route is in it.
	groupVerificationAddress = "verification_address"
	// defaultVerificationStartsPerIP is that budget an hour, and its burst;
	// KOALA_VERIFICATION_STARTS_PER_IP_HOUR overrides it.
	defaultVerificationStartsPerIP = 20
)

// perMinute is a bucket of burst requests that refills n per minute.
func perMinute(n, burst int) httpx.Rate {
	return httpx.Rate{Every: time.Minute / time.Duration(n), Burst: burst}
}

// perHour is a bucket of burst requests that refills n per hour.
func perHour(n, burst int) httpx.Rate {
	return httpx.Rate{Every: time.Hour / time.Duration(n), Burst: burst}
}

// serveRateLimitGroups are rateLimitGroups with cfg's overrides: the
// per-address budgets of registration and verification starts are
// configurable.
func serveRateLimitGroups(cfg config.Config) map[string]httpx.GroupLimits {
	groups := rateLimitGroups()
	if n := cfg.Mail.RegistrationStartsPerIP; n > 0 {
		groups[groupRegistrationAddress] = httpx.GroupLimits{PerCustom: perHour(n, n)}
	}
	if n := cfg.Verification.StartsPerIP; n > 0 {
		groups[groupVerificationAddress] = httpx.GroupLimits{PerCustom: perHour(n, n)}
	}
	return groups
}

// rateLimitGroups are the production limits (proposal, not measured);
// TestRateLimitGroups compares them with ADR-0021's table.
func rateLimitGroups() map[string]httpx.GroupLimits {
	return map[string]httpx.GroupLimits{
		httpx.GroupRead:  {PerIP: perMinute(600, 120), PerIdentity: perMinute(300, 60)},
		httpx.GroupWrite: {PerIP: perMinute(300, 60), PerIdentity: perMinute(60, 20)},
		// Login, registration, OTP and password reset. The module adds a
		// KeyCustom bucket per (hashed target, address) (Java: path + IP +
		// target).
		groupAuth:     {PerIP: perMinute(30, 10), PerCustom: perMinute(10, 5)},
		groupUpload:   {PerIP: perMinute(100, 20), PerIdentity: perMinute(20, 5)},
		groupMessage:  {PerIP: perMinute(150, 30), PerIdentity: perMinute(30, 10)},
		groupSitemap:  {PerIP: perMinute(30, 10)},
		groupBoarding: {PerIP: perMinute(600, 60), PerIdentity: perMinute(120, 20)},
		// A sign-in target from any address; the (target, address) bucket
		// is auth's PerCustom (ADR-0025 §8).
		groupAuthAccount: {PerCustom: perMinute(30, 10)},
		// Registration starts from one client address (an IPv6 /64), on
		// top of auth's per-address limit (ADR-0032 Karar 6).
		groupRegistrationAddress: {PerCustom: perHour(defaultRegistrationStartsPerIP, defaultRegistrationStartsPerIP)},
		// Verification starts from one client address, on top of auth's
		// per-address limit (ADR-0033 Karar 4).
		groupVerificationAddress: {PerCustom: perHour(defaultVerificationStartsPerIP, defaultVerificationStartsPerIP)},
	}
}
