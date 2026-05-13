package attest

import (
	"crypto/x509"
	_ "embed"
)

//go:embed apple_root_ca.pem
var appleAppAttestRootPEM []byte

// AppleAppAttestRootPool returns a CertPool seeded with Apple's App Attestation
// Root CA (NOT the generic Apple Root or DeviceCheck root — SPEC §6.3).
//
// The PEM is pinned at compile time from
// https://www.apple.com/certificateauthority/Apple_App_Attestation_Root_CA.pem.
//
// SHA256(PEM bytes) = c778d09ac341f7fd9f8f3b19e2b815af6aed4ad4490e1e92c05cb355212a5013
// (verified at commit time; the bytes are the trust anchor for every production
// attestation, so the hash is captured here as a tripwire — if the on-disk
// PEM ever drifts, the assertion in TestAppleRootPoolBytes will catch it).
func AppleAppAttestRootPool() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(appleAppAttestRootPEM) {
		panic("attest: embedded Apple App Attestation Root CA PEM is invalid")
	}
	return pool
}
