package attest

import "encoding/binary"

// authenticatorData byte layout (WebAuthn-compatible, per FIDO2 CTAP2):
//
//	[0:32]   rpIDHash         SHA256("TeamID.BundleID")
//	[32]     flags            bit 0x40 = AT (attested credential data present)
//	[33:37]  signCount        big-endian uint32
//	if AT (attestation only):
//	  [37:53]   aaguid              16 bytes
//	  [53:55]   credentialIdLength  big-endian uint16
//	  [55:55+L] credentialId        L bytes
//	  [55+L:]   credentialPublicKey COSE_Key CBOR (server reads pubkey from
//	                                cert, not from here — we ignore it)
//
// Apple's assertion authData is exactly 37 bytes (no AT block).

const flagAT byte = 0x40

type parsedAuthData struct {
	RPIDHash     []byte // 32 bytes
	Flags        byte
	Counter      uint32
	AAGUID       []byte // 16 bytes; nil when AT flag not set
	CredentialID []byte // variable length; nil when AT flag not set
}

// parseAuthDataAttestation parses an attestation authData. Requires the AT bit
// to be set and the attested credential data block to be parsable.
func parseAuthDataAttestation(b []byte) (*parsedAuthData, error) {
	p, err := parseAuthDataCommon(b)
	if err != nil {
		return nil, err
	}
	if p.Flags&flagAT == 0 {
		return nil, ErrBadCBOR
	}
	if p.AAGUID == nil || p.CredentialID == nil {
		return nil, ErrBadCBOR
	}
	return p, nil
}

// parseAuthDataAssertion parses an assertion authData. Apple emits 37 bytes,
// no AT block. We only consume the first 37 bytes; any trailing data is
// treated as malformed.
func parseAuthDataAssertion(b []byte) (*parsedAuthData, error) {
	if len(b) != 37 {
		return nil, ErrBadCBOR
	}
	return &parsedAuthData{
		RPIDHash: b[0:32],
		Flags:    b[32],
		Counter:  binary.BigEndian.Uint32(b[33:37]),
	}, nil
}

func parseAuthDataCommon(b []byte) (*parsedAuthData, error) {
	if len(b) < 37 {
		return nil, ErrBadCBOR
	}
	p := &parsedAuthData{
		RPIDHash: b[0:32],
		Flags:    b[32],
		Counter:  binary.BigEndian.Uint32(b[33:37]),
	}
	if p.Flags&flagAT == 0 {
		return p, nil
	}
	if len(b) < 55 {
		return nil, ErrBadCBOR
	}
	p.AAGUID = b[37:53]
	credIDLen := int(binary.BigEndian.Uint16(b[53:55]))
	if len(b) < 55+credIDLen {
		return nil, ErrBadCBOR
	}
	p.CredentialID = b[55 : 55+credIDLen]
	return p, nil
}
