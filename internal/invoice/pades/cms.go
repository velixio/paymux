package pades

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"sort"
	"time"
)

var (
	oidData          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidSHA256        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidSigningTime   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 5}
	oidSigningCertV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	oidSHA256WithRSA = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidECDSASHA256   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
)

// der wraps content in a DER element.
func der(class, tag int, compound bool, content []byte) []byte {
	b, err := asn1.Marshal(asn1.RawValue{Class: class, Tag: tag, IsCompound: compound, Bytes: content})
	if err != nil {
		panic("pades: " + err.Error()) // cannot happen: the inputs are byte slices
	}
	return b
}

func seq(parts ...[]byte) []byte {
	var all []byte
	for _, p := range parts {
		all = append(all, p...)
	}
	return der(asn1.ClassUniversal, asn1.TagSequence, true, all)
}

func set(parts ...[]byte) []byte {
	var all []byte
	for _, p := range parts {
		all = append(all, p...)
	}
	return der(asn1.ClassUniversal, asn1.TagSet, true, all)
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic("pades: " + err.Error())
	}
	return b
}

func algorithm(oid asn1.ObjectIdentifier, nullParams bool) []byte {
	if nullParams {
		return seq(must(asn1.Marshal(oid)), []byte{0x05, 0x00})
	}
	return seq(must(asn1.Marshal(oid)))
}

func attribute(oid asn1.ObjectIdentifier, value []byte) []byte {
	return seq(must(asn1.Marshal(oid)), set(value))
}

// cms builds the detached CAdES-B-B SignedData over the signed byte ranges.
func (s *Signer) cms(signed []byte, at time.Time) ([]byte, error) {
	digest := sha256.Sum256(signed)
	certHash := sha256.Sum256(s.cert.Raw)

	// signingCertificateV2 (RFC 5035): binds the signature to the certificate.
	essCertID := seq(must(asn1.Marshal(certHash[:]))) // hashAlgorithm defaults to SHA-256
	signingCert := seq(seq(essCertID))

	attrs := [][]byte{
		attribute(oidContentType, must(asn1.Marshal(oidData))),
		attribute(oidMessageDigest, must(asn1.Marshal(digest[:]))),
		attribute(oidSigningTime, must(asn1.Marshal(at))),
		attribute(oidSigningCertV2, signingCert),
	}
	// DER requires the members of a SET OF to be sorted by their encoding.
	sort.Slice(attrs, func(i, j int) bool { return string(attrs[i]) < string(attrs[j]) })
	var attrBytes []byte
	for _, a := range attrs {
		attrBytes = append(attrBytes, a...)
	}
	// The signature is over the attributes encoded as a SET (tag 0x31), which
	// travel inside the SignerInfo under the implicit tag [0] (0xA0).
	toSign := der(asn1.ClassUniversal, asn1.TagSet, true, attrBytes)
	attrDigest := sha256.Sum256(toSign)

	var sigOID asn1.ObjectIdentifier
	var params bool
	switch s.key.Public().(type) {
	case *rsa.PublicKey:
		sigOID, params = oidSHA256WithRSA, true
	case *ecdsa.PublicKey:
		sigOID = oidECDSASHA256
	default:
		return nil, errors.New("pades: unsupported key type")
	}
	sig, err := s.key.Sign(rand.Reader, attrDigest[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}

	serial, err := asn1.Marshal(s.cert.SerialNumber)
	if err != nil {
		return nil, err
	}
	signerInfo := seq(
		must(asn1.Marshal(1)),
		seq(s.cert.RawIssuer, serial), // IssuerAndSerialNumber
		algorithm(oidSHA256, true),
		der(asn1.ClassContextSpecific, 0, true, attrBytes),
		algorithm(sigOID, params),
		must(asn1.Marshal(sig)),
	)

	var certs []byte
	certs = append(certs, s.cert.Raw...)
	for _, c := range s.chain {
		certs = append(certs, c.Raw...)
	}
	signedData := seq(
		must(asn1.Marshal(1)),
		set(algorithm(oidSHA256, true)),
		seq(must(asn1.Marshal(oidData))), // detached: no content
		der(asn1.ClassContextSpecific, 0, true, certs),
		set(signerInfo),
	)
	return seq(must(asn1.Marshal(oidSignedData)), der(asn1.ClassContextSpecific, 0, true, signedData)), nil
}
