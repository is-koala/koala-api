//go:build conformance

package authz_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/authz"
)

// The OpenFGA conformance runner (plan §5.6-5, ADR-0014, ADR-0024). It
// uploads model.json and the matrix tuples to a real OpenFGA server and
// runs every [go, openfga] row on both engines. Any difference between
// the two engines, or between an engine and the matrix's expected
// decision, fails the test.
//
// It runs in CI's authz-conformance job (make test-conformance) against
// the openfga/openfga service container; FGA_API_URL (the name OpenFGA's
// CLI uses) points at its HTTP API. Not for the shared Mac.

const (
	openfgaReadyTimeout = 90 * time.Second
	openfgaCallTimeout  = 10 * time.Second
	// OpenFGA's default OPENFGA_MAX_TUPLES_PER_WRITE.
	openfgaWriteBatch = 100
)

func TestConformanceWithOpenFGA(t *testing.T) {
	t.Parallel()
	base := strings.TrimRight(os.Getenv("FGA_API_URL"), "/") //nolint:forbidigo // test harness: where CI's OpenFGA container listens, like libpq's PGHOST for dbtest
	if base == "" {
		t.Fatal("FGA_API_URL is not set; the conformance runner needs an OpenFGA server (CI job authz-conformance)")
	}
	ctx := context.Background()
	fga := &openfgaClient{base: base, http: &http.Client{Timeout: openfgaCallTimeout}}
	fga.waitReady(ctx, t)

	modelJSON, err := os.ReadFile("model.json")
	if err != nil {
		t.Fatal(err)
	}
	mx := loadMatrix(t)
	local := newMatrixEvaluator(t, mx)
	tuples := fixtureTuples(t, mx)

	fga.createStore(ctx, t)
	fga.writeModel(ctx, t, modelJSON)
	for start := 0; start < len(tuples); start += openfgaWriteBatch {
		fga.writeTuples(ctx, t, tuples[start:min(start+openfgaWriteBatch, len(tuples))])
	}

	type typeResult struct{ ran, agreed int }
	byType := map[string]*typeResult{}
	var notRun []string
	ran, agreed := 0, 0
	for i := range mx.Cases {
		c := &mx.Cases[i]
		if reason := notRunReason(mx, c); reason != "" {
			notRun = append(notRun, c.ID+" ("+c.Action+"): "+reason)
			continue
		}
		objectType, relation, ok := mx.Relation(c.Action)
		if !ok {
			t.Fatalf("%s: action %s is not a relation", c.ID, c.Action)
		}
		s, ok := fixtureSubject(t, c.Subject)
		if !ok {
			t.Fatalf("%s: anonymous subject in a [go, openfga] row", c.ID)
		}
		o := fixtureObject(t, c.Object)
		d, err := local.Check(ctx, s, authz.Action(relation), o)
		if err != nil {
			t.Fatalf("%s: Go evaluator: %v", c.ID, err)
		}
		remote := fga.check(ctx, t, s.String(), relation, o.String())

		tr := byType[objectType]
		if tr == nil {
			tr = &typeResult{}
			byType[objectType] = tr
		}
		ran++
		tr.ran++
		switch {
		case d.Allowed != remote:
			t.Errorf("%s %s %s#%s on %s: Go %v, OpenFGA %v", c.ID, c.Subject, objectType, relation, c.Object, d.Allowed, remote)
		case remote != c.Allowed():
			t.Errorf("%s %s %s#%s on %s: both engines %v, matrix expects %s", c.ID, c.Subject, objectType, relation, c.Object, remote, c.Expect)
		default:
			agreed++
			tr.agreed++
		}
	}

	types := make([]string, 0, len(byType))
	for typ := range byType {
		types = append(types, typ)
	}
	sort.Strings(types)
	var report strings.Builder
	fmt.Fprintf(&report, "OpenFGA conformance: %d rows ran on both engines, %d agreed with each other and with the matrix\n", ran, agreed)
	for _, typ := range types {
		tr := byType[typ]
		status := "green"
		if tr.agreed != tr.ran {
			status = "RED"
		}
		fmt.Fprintf(&report, "  %-20s %3d/%-3d %s\n", typ, tr.agreed, tr.ran, status)
	}
	fmt.Fprintf(&report, "not run on both engines: %d rows\n", len(notRun))
	for _, line := range notRun {
		fmt.Fprintf(&report, "  %s\n", line)
	}
	t.Log("\n" + report.String())
	if ran == 0 {
		t.Fatal("no row ran on both engines")
	}
}

// openfgaClient is a minimal client of OpenFGA's HTTP API, written on the
// standard library (openfga/go-sdk is not an approved dependency).
type openfgaClient struct {
	base    string
	http    *http.Client
	storeID string
	modelID string
}

func (c *openfgaClient) waitReady(ctx context.Context, t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(openfgaReadyTimeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("OpenFGA at %s not ready after %s (last error: %v)", c.base, openfgaReadyTimeout, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// post sends a JSON body and decodes the JSON answer into out.
func (c *openfgaClient) post(ctx context.Context, t *testing.T, path string, body []byte, out any) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("POST %s: read body: %v", path, err)
	}
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST %s: %s: %s", path, resp.Status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("POST %s: decode %s: %v", path, raw, err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (c *openfgaClient) createStore(ctx context.Context, t *testing.T) {
	t.Helper()
	var resp struct {
		ID string `json:"id"`
	}
	c.post(ctx, t, "/stores", mustJSON(t, map[string]string{"name": "koala-conformance"}), &resp)
	if resp.ID == "" {
		t.Fatal("CreateStore returned no id")
	}
	c.storeID = resp.ID
}

func (c *openfgaClient) writeModel(ctx context.Context, t *testing.T, model []byte) {
	t.Helper()
	// model.json is the request body as it is: openfga/language writes the
	// protobuf JSON names OpenFGA's HTTP API reads.
	var resp map[string]any
	c.post(ctx, t, "/stores/"+c.storeID+"/authorization-models", model, &resp)
	for _, k := range []string{"authorization_model_id", "authorizationModelId"} {
		if id, ok := resp[k].(string); ok && id != "" {
			c.modelID = id
			return
		}
	}
	t.Fatalf("WriteAuthorizationModel returned no id: %v", resp)
}

type tupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

func (c *openfgaClient) writeTuples(ctx context.Context, t *testing.T, tuples []authz.Tuple) {
	t.Helper()
	keys := make([]tupleKey, 0, len(tuples))
	for _, tp := range tuples {
		keys = append(keys, tupleKey{User: tp.User.String(), Relation: tp.Relation, Object: tp.Object.String()})
	}
	body := map[string]any{
		"authorization_model_id": c.modelID,
		"writes":                 map[string]any{"tuple_keys": keys},
	}
	c.post(ctx, t, "/stores/"+c.storeID+"/write", mustJSON(t, body), nil)
}

func (c *openfgaClient) check(ctx context.Context, t *testing.T, user, relation, object string) bool {
	t.Helper()
	body := map[string]any{
		"authorization_model_id": c.modelID,
		"tuple_key":              tupleKey{User: user, Relation: relation, Object: object},
		"consistency":            "HIGHER_CONSISTENCY",
	}
	var resp struct {
		Allowed *bool `json:"allowed"`
	}
	c.post(ctx, t, "/stores/"+c.storeID+"/check", mustJSON(t, body), &resp)
	if resp.Allowed == nil {
		// protojson omits false; an absent field is a denial.
		return false
	}
	return *resp.Allowed
}
