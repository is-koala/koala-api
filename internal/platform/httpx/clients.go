package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/appversion"
)

// Client headers of the contract (api/components/headers.yaml, ADR-0005
// madde 7, ADR-0021).
const (
	HeaderClientPlatform    = "X-Client-Platform"
	HeaderAppVersion        = "X-App-Version"
	HeaderMinimumAppVersion = "X-Minimum-App-Version"
	HeaderDeviceID          = "X-Device-Id"
)

// CodeAppVersionUnsupported answers a mobile build older than the
// configured minimum (ADR-0019 Karar 6).
const CodeAppVersionUnsupported = "APP_VERSION_UNSUPPORTED"

// AdminPathPrefix is the staff API (plan §3.3: a separate chain). Staff
// clients send no device or version.
const AdminPathPrefix = "/v1/admin"

// ClientPlatform is the X-Client-Platform value: the contract's
// ClientPlatform enum.
type ClientPlatform string

// Platforms.
const (
	PlatformIOS     ClientPlatform = "IOS"
	PlatformAndroid ClientPlatform = "ANDROID"
	PlatformWeb     ClientPlatform = "WEB"
	PlatformAdmin   ClientPlatform = "ADMIN"
)

// mobile reports whether p ships through an app store, where an old build
// can stay installed; only those platforms meet the version gate.
func (p ClientPlatform) mobile() bool { return p == PlatformIOS || p == PlatformAndroid }

// Device id bounds (Java's DeviceIdentity rule, which the mobile and web
// clients already follow: [A-Za-z0-9._:-]{8,128}).
const (
	minDeviceIDLen = 8
	maxDeviceIDLen = 128
)

// ClientPolicy is the client header rule of ADR-0021.
type ClientPolicy struct {
	// MinAppVersion is the oldest IOS or ANDROID build that is served.
	MinAppVersion appversion.Version
	// ExemptRoutes are route patterns that Koala's own clients do not call
	// (a payment provider's callback, an e-mail tracking pixel). They are
	// served without client headers. Health endpoints are always exempt.
	ExemptRoutes []string
}

type clientKey struct{}

type clientInfo struct {
	platform ClientPlatform
	deviceID string
}

// ClientPlatformFrom returns the platform the client gate accepted, in
// its canonical upper-case form, or "" for an exempt route.
//
// The value is the client's own claim. It chooses which header rule
// applies and may be logged or counted; it is never an authorization
// input: a decision that depends on "this is the admin panel" or "this is
// a mobile app" comes from the authenticated principal (token class,
// session), not from this header (ADR-0021).
func ClientPlatformFrom(ctx context.Context) ClientPlatform {
	c, _ := ctx.Value(clientKey{}).(clientInfo)
	return c.platform
}

// DeviceIDFrom returns the X-Device-Id the client gate accepted, or "".
// Faz 1's authentication binds it to the session's device.
func DeviceIDFrom(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(clientInfo)
	return c.deviceID
}

// clientGate enforces ClientPolicy. It runs in the root chain, before
// routing, so Router.Stream routes (WebSocket) meet it too; it finds the
// route with the pattern the router looked up for the request.
//
//	route                         platform          X-Device-Id   X-App-Version
//	unmatched, health, exempt     not read          not read      not read
//	/v1/admin…                    absent or ADMIN   not read      not read
//	everything else               IOS, ANDROID      required      ≥ MinAppVersion
//	                              WEB               required      not read
//
// A browser cannot set headers on a WebSocket handshake. A stream route's
// upgrade request with an Origin and no X-Client-Platform is therefore a
// web client; its device binding comes from the ticket (ADR-0018, ADR-0022).
type clientGate struct {
	policy   ClientPolicy
	exempt   map[string]bool
	isStream func(pattern string) bool
	metrics  *Metrics
	logger   *slog.Logger
}

func newClientGate(policy ClientPolicy, isStream func(string) bool, metrics *Metrics, logger *slog.Logger) *clientGate {
	exempt := map[string]bool{PathLivez: true, PathReadyz: true}
	for _, p := range policy.ExemptRoutes {
		exempt[p] = true
	}
	return &clientGate{policy: policy, exempt: exempt, isStream: isStream, metrics: metrics, logger: logger}
}

func (g *clientGate) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, err := g.check(r)
		if err != nil {
			if apperr.From(err).Code == CodeAppVersionUnsupported {
				w.Header().Set(HeaderMinimumAppVersion, g.policy.MinAppVersion.String())
			}
			WriteError(w, r, g.logger, err)
			return
		}
		if info.platform != "" {
			r = r.WithContext(context.WithValue(r.Context(), clientKey{}, info))
		}
		next.ServeHTTP(w, r)
	})
}

func (g *clientGate) check(r *http.Request) (clientInfo, error) {
	pattern := lookedUpRoute(r)
	if pattern == "" || g.exempt[pattern] {
		return clientInfo{}, nil
	}
	h := r.Header
	platform := canonicalPlatform(h.Get(HeaderClientPlatform))

	if pattern == AdminPathPrefix || strings.HasPrefix(pattern, AdminPathPrefix+"/") {
		if platform != "" && platform != PlatformAdmin {
			return clientInfo{}, headerValueInvalid(HeaderClientPlatform)
		}
		return clientInfo{platform: PlatformAdmin}, nil
	}

	device := h.Get(HeaderDeviceID)
	if platform == "" && g.isStream(pattern) && h.Get("Origin") != "" && headerHasToken(h, "Upgrade", "websocket") {
		if device != "" && !validDeviceID(device) {
			return clientInfo{}, headerValueInvalid(HeaderDeviceID)
		}
		return clientInfo{platform: PlatformWeb, deviceID: device}, nil
	}

	var fields []apperr.FieldError
	switch platform {
	case PlatformIOS, PlatformAndroid, PlatformWeb:
	case "":
		fields = append(fields, headerField(HeaderClientPlatform, CodeFieldRequired))
	case PlatformAdmin:
		// Staff clients only call /v1/admin; elsewhere ADMIN would skip
		// the device and version rules.
		fields = append(fields, headerField(HeaderClientPlatform, CodeFieldValueInvalid))
	default:
		fields = append(fields, headerField(HeaderClientPlatform, CodeFieldValueInvalid))
	}
	if platform.mobile() {
		// An outdated build is told to update before anything else: it may
		// predate the other headers' rules too.
		v, err := appversion.Parse(h.Get(HeaderAppVersion))
		if err != nil || v.Less(g.policy.MinAppVersion) {
			g.metrics.appVersionRejected(platform)
			return clientInfo{}, apperr.New(apperr.KindInvalid, CodeAppVersionUnsupported)
		}
	}
	switch {
	case device == "":
		fields = append(fields, headerField(HeaderDeviceID, CodeFieldRequired))
	case !validDeviceID(device):
		fields = append(fields, headerField(HeaderDeviceID, CodeFieldValueInvalid))
	}
	if len(fields) > 0 {
		return clientInfo{}, apperr.New(apperr.KindInvalid, CodeRequestInvalid).WithFields(fields...)
	}
	return clientInfo{platform: platform, deviceID: device}, nil
}

// canonicalPlatform upper-cases an ASCII header value; header values are
// matched case-insensitively (the web client sends "web") and the
// contract's enum is the upper-case form. A non-ASCII byte is kept as is,
// so it can never fold into a known value (Unicode's "ı" upper-cases to
// "I").
func canonicalPlatform(v string) ClientPlatform {
	b := []byte(strings.TrimSpace(v))
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return ClientPlatform(b)
}

func headerField(name string, code apperr.Code) apperr.FieldError {
	return apperr.FieldError{Parameter: name, In: "HEADER", Code: code}
}

func headerValueInvalid(name string) error {
	return apperr.New(apperr.KindInvalid, CodeRequestInvalid).WithFields(headerField(name, CodeFieldValueInvalid))
}

// validDeviceID applies [A-Za-z0-9._:-]{8,128}.
func validDeviceID(id string) bool {
	if len(id) < minDeviceIDLen || len(id) > maxDeviceIDLen {
		return false
	}
	for _, c := range []byte(id) {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// headerHasToken reports whether a comma-separated header contains token,
// ignoring case.
func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
