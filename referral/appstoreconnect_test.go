package referral

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The App Store Connect client against an httptest stand-in. Nothing here
// talks to Apple.

const (
	testASCKeyID  = "TESTKEY001"
	testASCIssuer = "69a6de7e-test-issuer"
)

var ascNow = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)

func testECKeyPEM(t *testing.T, curve elliptic.Curve) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newTestASC(t *testing.T, h http.HandlerFunc) (*ASCClient, *httptest.Server, *ecdsa.PrivateKey) {
	t.Helper()
	key, p8 := testECKeyPEM(t, elliptic.P256())
	c, err := NewASCClient(testASCKeyID, testASCIssuer, p8)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c.SetBaseURL(srv.URL)
	c.now = func() time.Time { return ascNow }
	return c, srv, key
}

// checkASCToken verifies the bearer token the way Apple would: ES256 over
// header.claims with r||s, and the documented header and claims.
func checkASCToken(t *testing.T, auth string, pub *ecdsa.PublicKey) {
	t.Helper()
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q", auth)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	enc := base64.RawURLEncoding
	var header map[string]any
	var claims map[string]any
	for i, out := range []*map[string]any{&header, &claims} {
		raw, err := enc.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
	}
	if !reflect.DeepEqual(header, map[string]any{"alg": "ES256", "kid": testASCKeyID, "typ": "JWT"}) {
		t.Errorf("header = %v", header)
	}
	iat, exp := claims["iat"].(float64), claims["exp"].(float64)
	if claims["iss"] != testASCIssuer || claims["aud"] != "appstoreconnect-v1" || int64(iat) != ascNow.Unix() || exp <= iat || exp-iat > 1200 || len(claims) != 4 {
		t.Errorf("claims = %v", claims)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %d bytes, %v (want raw r||s, 64)", len(sig), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Error("signature does not verify with the key's public half")
	}
}

func TestASCClientCreateSendsTheDocumentedRequest(t *testing.T) {
	var c *ASCClient
	var key *ecdsa.PrivateKey
	c, _, key = newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/subscriptionOfferCodeOneTimeUseCodes" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		checkASCToken(t, r.Header.Get("Authorization"), &key.PublicKey)
		var got any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		var want any
		_ = json.Unmarshal([]byte(`{"data":{"type":"subscriptionOfferCodeOneTimeUseCodes",
			"attributes":{"numberOfCodes":10,"expirationDate":"2027-04-17","environment":"SANDBOX"},
			"relationships":{"offerCode":{"data":{"type":"subscriptionOfferCodes","id":"offer-test-1"}}}}}`), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("body = %s", raw)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"type":"subscriptionOfferCodeOneTimeUseCodes","id":"622700","attributes":{"numberOfCodes":10}}}`))
	})
	id, err := c.CreateOneTimeCodes(context.Background(), CreateCodesRequest{OfferID: "offer-test-1", NumberOfCodes: 10, ExpirationDate: "2027-04-17", Environment: EnvSandbox})
	if err != nil || id != "622700" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestASCClientCreateOutcomes(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		uncertain bool
		kind      string
		detail    string
	}{
		{401, `{"errors":[{"status":"401","code":"NOT_AUTHORIZED","title":"Authentication credentials are missing or invalid.","detail":"Provide a properly configured and signed bearer token."}]}`, false, "http_401", "NOT_AUTHORIZED — Authentication credentials are missing or invalid."},
		{403, `{"errors":[{"code":"FORBIDDEN_ERROR","title":"This request is forbidden for security reasons"}]}`, false, "http_403", "FORBIDDEN_ERROR"},
		{409, `{"errors":[{"code":"ENTITY_ERROR.ATTRIBUTE.INVALID","detail":"quota"}]}`, false, "http_409", "quota"},
		{422, ``, false, "http_422", ""},
		{429, ``, false, "http_429", ""},
		{302, ``, false, "http_302", ""},
		{500, `{"errors":[{"code":"UNEXPECTED_ERROR"}]}`, true, "http_500", "UNEXPECTED_ERROR"},
		{503, ``, true, "http_503", ""},
		{201, ``, true, "http_201", "no batch id"},
		{201, `{"data":{}}`, true, "http_201", "no batch id"},
	}
	for _, tc := range cases {
		c, _, _ := newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if tc.status == 302 {
				w.Header().Set("Location", "https://elsewhere.example/")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.CreateOneTimeCodes(context.Background(), CreateCodesRequest{OfferID: "o", NumberOfCodes: 500, ExpirationDate: "2027-04-17", Environment: EnvProduction})
		var ae *ASCError
		if !errors.As(err, &ae) {
			t.Fatalf("%d: err = %v", tc.status, err)
		}
		uncertain, kind := ascOutcome(err)
		if uncertain != tc.uncertain || kind != tc.kind || !strings.Contains(ae.Detail, tc.detail) {
			t.Errorf("%d: uncertain=%v kind=%s detail=%q, want %v %s %q", tc.status, uncertain, kind, ae.Detail, tc.uncertain, tc.kind, tc.detail)
		}
	}
}

func TestASCClientTimeoutAfterTheRequestWentOutIsUncertain(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	c, _, _ := newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // Apple has the whole request
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	c.SetTimeout(80 * time.Millisecond)
	_, err := c.CreateOneTimeCodes(context.Background(), CreateCodesRequest{OfferID: "o", NumberOfCodes: 500, ExpirationDate: "2027-04-17", Environment: EnvProduction})
	if uncertain, kind := ascOutcome(err); !uncertain || kind != "no_answer" {
		t.Fatalf("client timeout: uncertain=%v kind=%s err=%v", uncertain, kind, err)
	}

	c.SetTimeout(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = c.CreateOneTimeCodes(ctx, CreateCodesRequest{OfferID: "o", NumberOfCodes: 500, ExpirationDate: "2027-04-17", Environment: EnvProduction})
	if uncertain, _ := ascOutcome(err); !uncertain || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context timeout: uncertain=%v err=%v", uncertain, err)
	}
}

func TestASCClientRequestThatNeverLeftIsNotUncertain(t *testing.T) {
	c, srv, _ := newTestASC(t, func(http.ResponseWriter, *http.Request) {})
	srv.Close() // connection refused from here on
	_, err := c.CreateOneTimeCodes(context.Background(), CreateCodesRequest{OfferID: "o", NumberOfCodes: 500, ExpirationDate: "2027-04-17", Environment: EnvProduction})
	if uncertain, kind := ascOutcome(err); uncertain || kind != "unreachable" {
		t.Fatalf("connection refused: uncertain=%v kind=%s err=%v", uncertain, kind, err)
	}
}

func TestASCClientListParsesAndFollowsNextOnTheSameHostOnly(t *testing.T) {
	var key *ecdsa.PrivateKey
	var c *ASCClient
	var srv *httptest.Server
	pages := 0
	c, srv, key = newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		checkASCToken(t, r.Header.Get("Authorization"), &key.PublicKey)
		if r.URL.Path != "/v1/subscriptionOfferCodes/offer-test-1/oneTimeUseCodes" {
			t.Errorf("path %s", r.URL.Path)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			if r.URL.Query().Get("limit") != "200" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			fmt.Fprintf(w, `{"data":[
				{"type":"subscriptionOfferCodeOneTimeUseCodes","id":"622436","attributes":{"numberOfCodes":500,"createdDate":"2026-10-20T03:06:33Z","expirationDate":"2027-04-17","active":true,"environment":"PRODUCTION"}},
				{"type":"subscriptionOfferCodeOneTimeUseCodes","id":"622548","attributes":{"numberOfCodes":10,"createdDate":"2026-10-20T03:20:01Z","expirationDate":"2027-04-17","active":true,"environment":"SANDBOX"}}],
				"links":{"self":"x","next":%q}}`, srv.URL+"/v1/subscriptionOfferCodes/offer-test-1/oneTimeUseCodes?cursor=abc&limit=200")
		case "abc":
			fmt.Fprint(w, `{"data":[
				{"type":"subscriptionOfferCodeOneTimeUseCodes","id":"622900","attributes":{"numberOfCodes":1000,"createdDate":"2026-10-22T08:00:00Z","expirationDate":"2027-04-19","active":false,"environment":"PRODUCTION"}}],
				"links":{"next":"https://elsewhere.example/v1/next"}}`)
		default:
			t.Errorf("unexpected page %s", r.URL.RawQuery)
		}
	})
	got, err := c.ListOneTimeCodeBatches(context.Background(), "offer-test-1")
	if err != nil || len(got) != 3 || pages != 2 {
		t.Fatalf("got %d batches over %d pages, err %v", len(got), pages, err)
	}
	want := OneTimeCodeBatch{ID: "622548", NumberOfCodes: 10, CreatedDate: time.Date(2026, 10, 20, 3, 20, 1, 0, time.UTC), ExpirationDate: "2027-04-17", Environment: EnvSandbox, Active: true}
	if !reflect.DeepEqual(got[1], want) {
		t.Errorf("batch = %+v, want %+v", got[1], want)
	}
	if got[2].ID != "622900" || got[2].Active || got[2].NumberOfCodes != 1000 {
		t.Errorf("page 2 = %+v", got[2])
	}
}

func TestASCClientValuesParsesTheCSV(t *testing.T) {
	c, _, _ := newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscriptionOfferCodeOneTimeUseCodes/622548/values" || r.Header.Get("Accept") != "text/csv" {
			t.Errorf("%s accept=%s", r.URL.Path, r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("\uFEFFKX7P2M9QAB\r\n\"ZZ4R8T2WQC\"\r\n\nnot-a-code\nKX7P2M9QAB\nlower12345\nSHORT\nAB12CD34EF,2027-04-17\n"))
	})
	got, lines, err := c.OneTimeCodeValues(context.Background(), "622548")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"KX7P2M9QAB", "ZZ4R8T2WQC", "AB12CD34EF"}; !reflect.DeepEqual(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
	// Non-empty lines: everything but the blank one (junk lines count).
	if lines != 7 {
		t.Errorf("lines = %d, want 7", lines)
	}

	c, _, _ = newTestASC(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, _, err := c.OneTimeCodeValues(context.Background(), "nope"); err == nil {
		t.Error("404 accepted")
	}
}

func TestASCClientListGivesEveryPageItsOwnTimeout(t *testing.T) {
	var srv *httptest.Server
	var c *ASCClient
	c, srv, _ = newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(70 * time.Millisecond) // each page well inside 150 ms; three together are not
		next := ""
		switch r.URL.Query().Get("cursor") {
		case "":
			next = srv.URL + "/v1/subscriptionOfferCodes/o/oneTimeUseCodes?cursor=2"
		case "2":
			next = srv.URL + "/v1/subscriptionOfferCodes/o/oneTimeUseCodes?cursor=3"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"id": "b" + r.URL.Query().Get("cursor"), "attributes": map[string]any{"numberOfCodes": 10, "createdDate": "2026-10-22T08:00:00Z"}}},
			"links": map[string]any{"next": next},
		})
	})
	c.SetTimeout(150 * time.Millisecond)
	got, err := c.ListOneTimeCodeBatches(context.Background(), "o")
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d batches, err %v (pages must not share one timeout)", len(got), err)
	}
}

func TestASCClientErrorsCarryNoToken(t *testing.T) {
	var seen string
	c, _, _ := newTestASC(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := c.CreateOneTimeCodes(context.Background(), CreateCodesRequest{OfferID: "o", NumberOfCodes: 500, ExpirationDate: "2027-04-17", Environment: EnvProduction})
	tok := strings.TrimPrefix(seen, "Bearer ")
	if err == nil || tok == "" || strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), "Bearer") {
		t.Errorf("err = %v", err)
	}
}

func TestNewASCClientRejectsUnusableKeys(t *testing.T) {
	_, p384 := testECKeyPEM(t, elliptic.P384())
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	garbage := []byte("-----BEGIN PRIVATE KEY-----\nTUlHVGFnRUFNQk1HQnlxR1NNNDlBZ0VHQ0NxR1NNNDlBd0VIQkhrd2R3SUJBUVFn\n-----END PRIVATE KEY-----\n")
	_, good := testECKeyPEM(t, elliptic.P256())
	cases := map[string]struct {
		kid, iss string
		p8       []byte
	}{
		"not a PEM":    {testASCKeyID, testASCIssuer, []byte("MIGTAgEAMBMGByqGSM49")},
		"garbage DER":  {testASCKeyID, testASCIssuer, garbage},
		"RSA key":      {testASCKeyID, testASCIssuer, rsaPEM},
		"P-384 key":    {testASCKeyID, testASCIssuer, p384},
		"no key id":    {"", testASCIssuer, good},
		"no issuer id": {testASCKeyID, " ", good},
	}
	for name, tc := range cases {
		c, err := NewASCClient(tc.kid, tc.iss, tc.p8)
		if err == nil || c != nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "MIG") || strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("%s: key material in the error: %v", name, err)
		}
	}
}

// --- end to end: the restock loop over the real client -------------------------------------

// fakeAppStoreConnect is just enough of the three endpoints for one offer.
type fakeAppStoreConnect struct {
	mu        sync.Mutex
	batches   []map[string]any
	values    map[string]string
	creates   int
	fail5xx   bool // answer 500 after making the batch
	createdAt time.Time
}

func (f *fakeAppStoreConnect) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptionOfferCodeOneTimeUseCodes":
			var in struct {
				Data struct {
					Attributes struct {
						NumberOfCodes  int    `json:"numberOfCodes"`
						ExpirationDate string `json:"expirationDate"`
						Environment    string `json:"environment"`
					} `json:"attributes"`
				} `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("create body: %v", err)
			}
			f.creates++
			id := fmt.Sprintf("6227%02d", f.creates)
			a := in.Data.Attributes
			f.batches = append(f.batches, map[string]any{"type": "subscriptionOfferCodeOneTimeUseCodes", "id": id, "attributes": map[string]any{
				"numberOfCodes": a.NumberOfCodes, "createdDate": f.createdAt.Format(time.RFC3339), "expirationDate": a.ExpirationDate, "active": true, "environment": a.Environment,
			}})
			var csv strings.Builder
			for i := 0; i < a.NumberOfCodes; i++ {
				fmt.Fprintf(&csv, "E%s%04d\n", id, i)
			}
			f.values[id] = csv.String()
			if f.fail5xx {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"data":{"type":"subscriptionOfferCodeOneTimeUseCodes","id":%q}}`, id)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/oneTimeUseCodes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": f.batches, "links": map[string]any{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/values"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/subscriptionOfferCodeOneTimeUseCodes/"), "/values")
			v, ok := f.values[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, v)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestRestockEndToEndOverTheRealClient(t *testing.T) {
	for _, fail5xx := range []bool{false, true} {
		apple := &fakeAppStoreConnect{values: map[string]string{}, fail5xx: fail5xx, createdAt: base}
		c, _, _ := newTestASC(t, apple.handler(t))
		store, al := newMemStore(), &fakeAlerter{}
		store.pool = lowPool(2)
		rs := restockOn(EnvSandbox)
		rs.BatchSize, rs.Below = 10, 5 // the staging setup
		store.rs.settings = rs
		svc := New(store, newFakeRC(), entID, okAuth, Options{Alerter: al, Restock: RestockConfig{API: c, OfferID: "offer-test-1", Environment: "sandbox"}})

		settleAt(svc, store, base)
		if fail5xx {
			// 502 after Apple made it: creating, reconciled next pass.
			if rows := store.restockRows(); rows[0].State != RestockCreating {
				t.Fatalf("5xx: %+v", rows[0])
			}
			settleAt(svc, store, base.Add(10*time.Minute))
		}
		rows := store.restockRows()
		if len(rows) != 1 || rows[0].State != RestockImported || rows[0].ImportedCount != 10 || rows[0].BatchID != "622701" || apple.creates != 1 {
			t.Fatalf("fail5xx=%v: rows %+v creates %d", fail5xx, rows, apple.creates)
		}
		if got := mailsWith(al, "[SANDBOX] Referral code restock: added 10 codes, expires "+wantExpiry); len(got) != 1 {
			t.Errorf("fail5xx=%v: mails %+v", fail5xx, al.sent)
		}
	}
}
