package fakes_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

// iyzicoVectors is the part of testdata/iyzico/vectors.json (plan §6.2-3)
// the fake is checked against.
type iyzicoVectors struct {
	Keys struct {
		SecretKey string `json:"secret_key"`
		APIKey    string `json:"api_key"`
	} `json:"test_anahtarlari"`
	Response struct {
		JavaTest []struct {
			ID        string            `json:"id"`
			Use       string            `json:"uc"`
			Fields    map[string]string `json:"yanit_alanlari"`
			Signature string            `json:"imza"`
		} `json:"java_test"`
		Normalization []struct {
			In  string `json:"girdi"`
			Out string `json:"cikti"`
		} `json:"turetilmis_normalizasyon"`
	} `json:"yanit_imzasi"`
	Request struct {
		Derived []struct {
			ID            string `json:"id"`
			RandomKey     string `json:"random_key"`
			URIPath       string `json:"uri_path"`
			Body          string `json:"json_body"`
			Signature     string `json:"imza"`
			Authorization string `json:"authorization"`
		} `json:"turetilmis"`
	} `json:"istek_imzasi"`
}

func loadIyzicoVectors(t *testing.T) iyzicoVectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "iyzico", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v iyzicoVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Response.JavaTest) == 0 || len(v.Request.Derived) == 0 || len(v.Response.Normalization) == 0 {
		t.Fatal("vectors.json: a section is empty; did its keys change?")
	}
	return v
}

func newIyzico(t *testing.T, v iyzicoVectors) *fakes.Iyzico {
	t.Helper()
	return fakes.NewIyzico(t, fakes.IyzicoKeys{APIKey: v.Keys.APIKey, SecretKey: v.Keys.SecretKey})
}

func iyzicoHeader(apiKey, randomKey, signature string) http.Header {
	auth := "apiKey:" + apiKey + "&randomKey:" + randomKey + "&signature:" + signature
	return http.Header{
		"Content-Type":  {"application/json"},
		"X-Iyzi-Rnd":    {randomKey},
		"Authorization": {"IYZWSv2 " + base64.StdEncoding.EncodeToString([]byte(auth))},
	}
}

func TestIyzicoAcceptsTheVectorRequests(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	for _, vec := range v.Request.Derived {
		t.Run(vec.ID, func(t *testing.T) {
			t.Parallel()
			f := newIyzico(t, v)
			h := iyzicoHeader(v.Keys.APIKey, vec.RandomKey, vec.Signature)
			if vec.Authorization != "" && h.Get("Authorization") != vec.Authorization {
				t.Fatalf("Authorization = %q, vector has %q", h.Get("Authorization"), vec.Authorization)
			}
			f.Enqueue(http.MethodPost, vec.URIPath, f.Success(nil))

			status, body := call(t, f.Client(), http.MethodPost, f.URL()+vec.URIPath, h, vec.Body)
			if status != http.StatusOK {
				t.Fatalf("status %d, body %s", status, body)
			}
			reqs := f.RequestsTo(http.MethodPost, vec.URIPath)
			if len(reqs) != 1 || string(reqs[0].Body) != vec.Body {
				t.Fatalf("recorded %+v", reqs)
			}
			if err := f.VerifyRequest(reqs[0]); err != nil {
				t.Fatalf("VerifyRequest: %v", err)
			}
		})
	}
}

func TestIyzicoRefusesABadlyAuthenticatedRequest(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	vec := v.Request.Derived[0]
	good := iyzicoHeader(v.Keys.APIKey, vec.RandomKey, vec.Signature)
	cases := map[string]struct {
		header http.Header
		body   string
	}{
		"body changed":         {good, vec.Body + " "},
		"wrong api key":        {iyzicoHeader("other-key", vec.RandomKey, vec.Signature), vec.Body},
		"wrong signature":      {iyzicoHeader(v.Keys.APIKey, vec.RandomKey, strings.Repeat("0", 64)), vec.Body},
		"no x-iyzi-rnd":        {http.Header{"Authorization": good["Authorization"]}, vec.Body},
		"rnd differs":          {http.Header{"Authorization": good["Authorization"], "X-Iyzi-Rnd": {"1"}}, vec.Body},
		"not IYZWSv2":          {http.Header{"Authorization": {"Bearer x"}, "X-Iyzi-Rnd": {vec.RandomKey}}, vec.Body},
		"malformed IYZWSv2":    {http.Header{"Authorization": {"IYZWSv2 !!"}, "X-Iyzi-Rnd": {vec.RandomKey}}, vec.Body},
		"upper-case signature": {iyzicoHeader(v.Keys.APIKey, vec.RandomKey, strings.ToUpper(vec.Signature)), vec.Body},
		"missing signature:":   {http.Header{"Authorization": {"IYZWSv2 " + base64.StdEncoding.EncodeToString([]byte("apiKey:a&randomKey:b"))}, "X-Iyzi-Rnd": {"b"}}, vec.Body},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newIyzico(t, v)
			f.ExpectAuthFailures()
			f.Enqueue(http.MethodPost, vec.URIPath, f.Success(map[string]any{"marker": "programmed"}))

			status, body := call(t, f.Client(), http.MethodPost, f.URL()+vec.URIPath, tc.header, tc.body)
			if status != http.StatusUnauthorized || decode(t, body)["status"] != "failure" {
				t.Fatalf("status %d, body %s; want 401 failure", status, body)
			}
			// The refused request consumed nothing: a good one gets the
			// programmed answer.
			status, body = call(t, f.Client(), http.MethodPost, f.URL()+vec.URIPath, good, vec.Body)
			if status != http.StatusOK || decode(t, body)["marker"] != "programmed" {
				t.Fatalf("after refusal: status %d, body %s", status, body)
			}
		})
	}
}

func TestIyzicoSignsResponsesLikeTheVectors(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	f := newIyzico(t, v)
	checked := 0
	for _, vec := range v.Response.JavaTest {
		if vec.Fields == nil {
			continue // IV-05: a tampered signature, a client-side case
		}
		var resp fakes.Response
		switch vec.Use {
		case "checkoutform_retrieve":
			resp = f.CheckoutFormRetrieved(fakes.CheckoutForm{
				PaymentStatus:  vec.Fields["paymentStatus"],
				PaymentID:      vec.Fields["paymentId"],
				Currency:       vec.Fields["currency"],
				BasketID:       vec.Fields["basketId"],
				ConversationID: vec.Fields["conversationId"],
				PaidPrice:      vec.Fields["paidPrice"],
				Price:          vec.Fields["price"],
				Token:          vec.Fields["token"],
			})
			if !strings.Contains(string(resp.Body), `"paidPrice":`+vec.Fields["paidPrice"]) {
				t.Fatalf("%s: paidPrice is not the JSON number %s: %s", vec.ID, vec.Fields["paidPrice"], resp.Body)
			}
		case "checkoutform_initialize":
			resp = f.CheckoutFormInitialized(vec.Fields["conversationId"], vec.Fields["token"], vec.Fields["paymentPageUrl"])
		default:
			t.Fatalf("%s: unknown use %q", vec.ID, vec.Use)
		}
		body := decode(t, resp.Body)
		if body["signature"] != vec.Signature {
			t.Fatalf("%s: signature %v, vector %s", vec.ID, body["signature"], vec.Signature)
		}
		if body["status"] != "success" {
			t.Fatalf("%s: status %v", vec.ID, body["status"])
		}
		checked++
	}
	if checked < 4 {
		t.Fatalf("checked %d response vectors, want IV-01..IV-04", checked)
	}
}

func TestIyzicoSignsNormalizedDecimals(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	f := newIyzico(t, v)
	for _, n := range v.Response.Normalization {
		resp := f.CheckoutFormRetrieved(fakes.CheckoutForm{
			PaymentStatus: "SUCCESS", PaymentID: "p", Currency: "TRY", BasketID: "b",
			ConversationID: "c", PaidPrice: n.In, Price: n.In, Token: "t",
		})
		m := hmac.New(sha256.New, []byte(v.Keys.SecretKey))
		_, _ = m.Write([]byte("SUCCESS:p:TRY:b:c:" + n.Out + ":" + n.Out + ":t"))
		if got, want := decode(t, resp.Body)["signature"], hex.EncodeToString(m.Sum(nil)); got != want {
			t.Fatalf("%s: signature does not cover %s", n.In, n.Out)
		}
	}
}

func TestIyzicoRetrieveRefusesANonDecimalPrice(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	f := newIyzico(t, v)
	for _, price := range []string{"", "1e3", "abc", "1.", ".5", "1.2.3"} {
		t.Run(price, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("price %q did not panic", price)
				}
			}()
			f.CheckoutFormRetrieved(fakes.CheckoutForm{PaidPrice: price, Price: "1"})
		})
	}
}

func TestIyzicoFailureAndSuccessBodies(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	f := newIyzico(t, v)
	fail := decode(t, f.Failure("10051", "card declined").Body)
	if fail["status"] != "failure" || fail["errorCode"] != "10051" || fail["errorMessage"] != "card declined" {
		t.Fatalf("Failure body %v", fail)
	}
	ok := decode(t, f.Success(map[string]any{"paymentId": "pay-1"}).Body)
	if ok["status"] != "success" || ok["paymentId"] != "pay-1" {
		t.Fatalf("Success body %v", ok)
	}
	if got := f.Sign("conv-1", "tok-1"); got != v.Response.JavaTest[2].Signature {
		t.Fatalf("Sign(conv-1, tok-1) = %s, want IV-03's signature", got)
	}
}

// Without ExpectAuthFailures a badly signed request fails the test: a
// client that signs wrongly must not pass because it handles the 401.
func TestIyzicoAuthFailureFailsTheTestByDefault(t *testing.T) {
	t.Parallel()
	v := loadIyzicoVectors(t)
	vec := v.Request.Derived[0]
	rec := &recordingTB{}
	t.Cleanup(func() { rec.finish() })
	f := fakes.NewIyzico(rec, fakes.IyzicoKeys{APIKey: v.Keys.APIKey, SecretKey: v.Keys.SecretKey})

	h := iyzicoHeader(v.Keys.APIKey, vec.RandomKey, vec.Signature)
	if status, _ := call(t, f.Client(), http.MethodPost, f.URL()+vec.URIPath, h, vec.Body+" "); status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", status)
	}
	errs := rec.finish()
	if len(errs) != 1 || !strings.Contains(errs[0], "failed authentication") {
		t.Fatalf("reported at cleanup: %q", errs)
	}
}
