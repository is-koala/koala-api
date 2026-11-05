package authz

import (
	"context"
	"fmt"
	"log/slog"
)

// MismatchCounter counts decisions on which the primary and the shadow
// checker disagree. Its production implementation is the Prometheus
// counter koala_authz_mismatch_total{object_type, action, primary} (plan
// §5.7-2), which arrives with the metrics package; until then nothing
// implements it outside tests.
type MismatchCounter interface {
	AuthzMismatch(objectType string, a Action, primaryAllowed bool)
}

// ShadowChecker is the dark mode of plan §5.7 (Slack's loopback and
// parallel comparison, core-backend's AUTHZ_ROLE_MODE=both): every check
// is answered by the primary checker; the shadow checker is asked the
// same question and a different answer, or a shadow error, is counted and
// logged. The shadow never changes a decision.
//
// Light mode is the same type with the two checkers swapped.
type ShadowChecker struct {
	primary Checker
	shadow  Checker
	counter MismatchCounter
	log     *slog.Logger
}

// NewShadowChecker returns a checker that answers from primary and
// compares with shadow.
func NewShadowChecker(primary, shadow Checker, counter MismatchCounter, log *slog.Logger) *ShadowChecker {
	return &ShadowChecker{primary: primary, shadow: shadow, counter: counter, log: log}
}

var _ Checker = (*ShadowChecker)(nil)

// Check answers from the primary checker and compares with the shadow.
// The shadow runs after the primary, in the same request; a production
// version would bound its latency or run it asynchronously.
func (c *ShadowChecker) Check(ctx context.Context, s Subject, a Action, o Object, opts ...CheckOption) (Decision, error) {
	d, err := c.primary.Check(ctx, s, a, o, opts...)
	if err != nil {
		return d, fmt.Errorf("authz primary: %w", err)
	}
	sd, serr := c.shadow.Check(ctx, s, a, o, opts...)
	c.compare(ctx, a, o, d, sd, serr)
	return d, nil
}

// CheckBatch answers from the primary checker and compares every object
// with the shadow.
func (c *ShadowChecker) CheckBatch(ctx context.Context, s Subject, a Action, objs []Object, opts ...CheckOption) (map[Object]Decision, error) {
	ds, err := c.primary.CheckBatch(ctx, s, a, objs, opts...)
	if err != nil {
		return ds, fmt.Errorf("authz primary: %w", err)
	}
	sds, serr := c.shadow.CheckBatch(ctx, s, a, objs, opts...)
	for _, o := range objs {
		c.compare(ctx, a, o, ds[o], sds[o], serr)
	}
	return ds, nil
}

func (c *ShadowChecker) compare(ctx context.Context, a Action, o Object, primary, shadow Decision, shadowErr error) {
	if shadowErr == nil && primary.Allowed == shadow.Allowed {
		return
	}
	c.counter.AuthzMismatch(o.Type, a, primary.Allowed)
	attrs := []slog.Attr{
		slog.String("object_type", o.Type),
		slog.String("object_id", o.ID),
		slog.String("action", string(a)),
		slog.Bool("primary_allowed", primary.Allowed),
		slog.Bool("shadow_allowed", shadow.Allowed),
	}
	if shadowErr != nil {
		attrs = append(attrs, slog.String("shadow_error", shadowErr.Error()))
	}
	c.log.LogAttrs(ctx, slog.LevelWarn, "authz shadow mismatch", attrs...)
}
