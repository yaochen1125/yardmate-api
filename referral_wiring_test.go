package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yaochen1125/yardmate-api/referral"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// testDeviceCheckPEM is a throwaway P-256 key as a .p8 PEM.
func testDeviceCheckPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func vaultOf(t *testing.T, text string) *secrets.Vault {
	t.Helper()
	v, err := secrets.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBuildDeviceCheckReadsTheKeyLikeAppleSignIn(t *testing.T) {
	pemText := testDeviceCheckPEM(t)
	// secrets.env is line-based: the PEM goes in as ONE line with literal \n,
	// exactly like APPLE_PRIVATE_KEY.
	oneLine := strings.ReplaceAll(strings.TrimSpace(pemText), "\n", `\n`)
	dc, mode := buildDeviceCheck(vaultOf(t, "DEVICECHECK_KEY_ID=W3YN83SB24\nAPPLE_TEAM_ID=PMX32RG52M\nDEVICECHECK_MODE=log\nDEVICECHECK_PRIVATE_KEY="+oneLine+"\n"))
	if dc == nil || mode != referral.DeviceCheckLog {
		t.Fatalf("inline key: client=%v mode=%s, want a client in log mode", dc != nil, mode)
	}

	// Or a path to the .p8.
	path := filepath.Join(t.TempDir(), "AuthKey_W3YN83SB24.p8")
	if err := os.WriteFile(path, []byte(pemText), 0o600); err != nil {
		t.Fatal(err)
	}
	dc, mode = buildDeviceCheck(vaultOf(t, "DEVICECHECK_KEY_ID=W3YN83SB24\nAPPLE_TEAM_ID=PMX32RG52M\nDEVICECHECK_PRIVATE_KEY_PATH="+path+"\n"))
	if dc == nil || mode != referral.DeviceCheckEnforce {
		t.Fatalf("key path: client=%v mode=%s, want a client, enforce by default", dc != nil, mode)
	}
}

func TestBuildDeviceCheckMissingPiecesFailClosed(t *testing.T) {
	oneLine := strings.ReplaceAll(strings.TrimSpace(testDeviceCheckPEM(t)), "\n", `\n`)
	cases := map[string]string{
		"no key id":  "APPLE_TEAM_ID=PMX32RG52M\nDEVICECHECK_PRIVATE_KEY=" + oneLine + "\n",
		"no team id": "DEVICECHECK_KEY_ID=W3YN83SB24\nDEVICECHECK_PRIVATE_KEY=" + oneLine + "\n",
		"no key":     "DEVICECHECK_KEY_ID=W3YN83SB24\nAPPLE_TEAM_ID=PMX32RG52M\n",
		"bad key":    "DEVICECHECK_KEY_ID=W3YN83SB24\nAPPLE_TEAM_ID=PMX32RG52M\nDEVICECHECK_PRIVATE_KEY=not-a-key\n",
		"odd mode":   "DEVICECHECK_MODE=off\n",
	}
	for name, text := range cases {
		dc, mode := buildDeviceCheck(vaultOf(t, text))
		if dc != nil || mode != referral.DeviceCheckEnforce {
			t.Errorf("%s: client=%v mode=%s, want none and enforce", name, dc != nil, mode)
		}
	}
	// The referral service still builds and says so.
	svc := buildReferralService(vaultOf(t, ""), nil, nil)
	if mode, ok := svc.DeviceCheck(); ok || mode != referral.DeviceCheckEnforce {
		t.Errorf("service DeviceCheck() = %s %v", mode, ok)
	}
}

func TestDeviceCheckAllowDevelopmentIsOffUnlessSet(t *testing.T) {
	for text, want := range map[string]bool{
		"":                                      false, // prod: never set
		"DEVICECHECK_ALLOW_DEVELOPMENT=false\n": false,
		"DEVICECHECK_ALLOW_DEVELOPMENT=maybe\n": false, // not a bool → off
		"DEVICECHECK_ALLOW_DEVELOPMENT=true\n":  true,  // staging
		"DEVICECHECK_ALLOW_DEVELOPMENT=1\n":     true,
	} {
		svc := buildReferralService(vaultOf(t, text), nil, nil)
		if got := svc.DeviceCheckAllowsDevelopment(); got != want {
			t.Errorf("%q: allow development = %v, want %v", text, got, want)
		}
	}
}

func TestBuildRestockConfigReadsTheFiveASCKeys(t *testing.T) {
	oneLine := strings.ReplaceAll(strings.TrimSpace(testDeviceCheckPEM(t)), "\n", `\n`)
	all := "ASC_KEY_ID=TESTKEY001\nASC_ISSUER_ID=69a6de7e-test\nASC_PRIVATE_KEY=" + oneLine +
		"\nASC_REFERRAL_OFFER_ID=offer-test-3\nASC_OFFER_CODE_ENVIRONMENT=sandbox\n"
	cfg := buildRestockConfig(vaultOf(t, all))
	if cfg.API == nil || len(cfg.Missing) != 0 || cfg.Environment != referral.EnvSandbox || cfg.OfferID != "offer-test-3" {
		t.Fatalf("all five: api=%v missing=%v env=%q offer=%q", cfg.API != nil, cfg.Missing, cfg.Environment, cfg.OfferID)
	}

	cases := map[string]struct {
		drop, want string
	}{
		"no key id":      {"ASC_KEY_ID=", "ASC_KEY_ID"},
		"no issuer":      {"ASC_ISSUER_ID=", "ASC_ISSUER_ID"},
		"no key":         {"ASC_PRIVATE_KEY=", "ASC_PRIVATE_KEY"},
		"no offer":       {"ASC_REFERRAL_OFFER_ID=", "ASC_REFERRAL_OFFER_ID"},
		"no environment": {"ASC_OFFER_CODE_ENVIRONMENT=", "ASC_OFFER_CODE_ENVIRONMENT"},
	}
	for name, c := range cases {
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(all), "\n") {
			if !strings.HasPrefix(l, c.drop) {
				lines = append(lines, l)
			}
		}
		cfg := buildRestockConfig(vaultOf(t, strings.Join(lines, "\n")+"\n"))
		if !strings.Contains(strings.Join(cfg.Missing, ","), c.want) {
			t.Errorf("%s: missing = %v", name, cfg.Missing)
		}
	}

	bad := strings.Replace(all, "ASC_OFFER_CODE_ENVIRONMENT=sandbox", "ASC_OFFER_CODE_ENVIRONMENT=prod", 1)
	if cfg := buildRestockConfig(vaultOf(t, bad)); cfg.Environment != "" || !strings.Contains(strings.Join(cfg.Missing, ","), "neither PRODUCTION nor SANDBOX") {
		t.Errorf("odd environment: %+v", cfg.Missing)
	}
	broken := strings.Replace(all, "ASC_PRIVATE_KEY="+oneLine, "ASC_PRIVATE_KEY=-----BEGIN PRIVATE KEY-----\\nnot-a-key\\n-----END PRIVATE KEY-----", 1)
	cfg = buildRestockConfig(vaultOf(t, broken))
	if cfg.API != nil || !strings.Contains(strings.Join(cfg.Missing, ","), "ASC_PRIVATE_KEY (unusable") || strings.Contains(strings.Join(cfg.Missing, ","), "not-a-key") {
		t.Errorf("broken key: api=%v missing=%v", cfg.API != nil, cfg.Missing)
	}
	// Nothing set (the switch is off by default): still a service.
	if cfg := buildRestockConfig(vaultOf(t, "")); cfg.API != nil || len(cfg.Missing) != 5 {
		t.Errorf("empty vault: %+v", cfg.Missing)
	}
}
