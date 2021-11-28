package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"github.com/smallstep/assert"
	"github.com/smallstep/certificates/acme"
	"github.com/smallstep/certificates/authority"
	"github.com/smallstep/certificates/authority/provisioner"
	"go.step.sm/crypto/jose"
	"go.step.sm/crypto/keyutil"
	"go.step.sm/crypto/x509util"
	"golang.org/x/crypto/ocsp"
)

// v is a utility function to return the pointer to an integer
func v(v int) *int {
	return &v
}

// generateCertKeyPair generates fresh x509 certificate/key pairs for testing
func generateCertKeyPair() (*x509.Certificate, crypto.Signer, error) {

	pub, priv, err := keyutil.GenerateKeyPair("EC", "P-256", 0)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1000000000000000000))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		Subject:      pkix.Name{CommonName: "Test ACME Revoke Certificate"},
		Issuer:       pkix.Name{CommonName: "Test ACME Revoke Certificate"},
		IsCA:         false,
		MaxPathLen:   0,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		NotBefore:    now,
		NotAfter:     now.Add(time.Hour),
		SerialNumber: serial,
	}

	signer, ok := priv.(crypto.Signer)
	if !ok {
		return nil, nil, errors.Errorf("result is not a crypto.Signer: type %T", priv)
	}

	cert, err := x509util.CreateCertificate(template, template, pub, signer)

	return cert, signer, err
}

var errUnsupportedKey = fmt.Errorf("unknown key type; only RSA and ECDSA are supported")

// keyID is the account identity provided by a CA during registration.
type keyID string

// noKeyID indicates that jwsEncodeJSON should compute and use JWK instead of a KID.
// See jwsEncodeJSON for details.
const noKeyID = keyID("")

// jwsEncodeJSON signs claimset using provided key and a nonce.
// The result is serialized in JSON format containing either kid or jwk
// fields based on the provided keyID value.
//
// If kid is non-empty, its quoted value is inserted in the protected head
// as "kid" field value. Otherwise, JWK is computed using jwkEncode and inserted
// as "jwk" field value. The "jwk" and "kid" fields are mutually exclusive.
//
// See https://tools.ietf.org/html/rfc7515#section-7.
//
// If nonce is empty, it will not be encoded into the header.
// Implementation taken from github.com/mholt/acmez, which seems to be based on
// https://github.com/golang/crypto/blob/master/acme/jws.go.
func jwsEncodeJSON(claimset interface{}, key crypto.Signer, kid keyID, nonce, u string) ([]byte, error) {
	alg, sha := jwsHasher(key.Public())
	if alg == "" || !sha.Available() {
		return nil, errUnsupportedKey
	}

	phead, err := jwsHead(alg, nonce, u, kid, key)
	if err != nil {
		return nil, err
	}

	var payload string
	if claimset != nil {
		cs, err := json.Marshal(claimset)
		if err != nil {
			return nil, err
		}
		payload = base64.RawURLEncoding.EncodeToString(cs)
	}

	payloadToSign := []byte(phead + "." + payload)
	hash := sha.New()
	_, _ = hash.Write(payloadToSign)
	digest := hash.Sum(nil)

	sig, err := jwsSign(key, sha, digest)
	if err != nil {
		return nil, err
	}

	return jwsFinal(sha, sig, phead, payload)
}

// jwsHasher indicates suitable JWS algorithm name and a hash function
// to use for signing a digest with the provided key.
// It returns ("", 0) if the key is not supported.
// Implementation taken from github.com/mholt/acmez, which seems to be based on
// https://github.com/golang/crypto/blob/master/acme/jws.go.
func jwsHasher(pub crypto.PublicKey) (string, crypto.Hash) {
	switch pub := pub.(type) {
	case *rsa.PublicKey:
		return "RS256", crypto.SHA256
	case *ecdsa.PublicKey:
		switch pub.Params().Name {
		case "P-256":
			return "ES256", crypto.SHA256
		case "P-384":
			return "ES384", crypto.SHA384
		case "P-521":
			return "ES512", crypto.SHA512
		}
	}
	return "", 0
}

// jwsSign signs the digest using the given key.
// The hash is unused for ECDSA keys.
//
// Note: non-stdlib crypto.Signer implementations are expected to return
// the signature in the format as specified in RFC7518.
// See https://tools.ietf.org/html/rfc7518 for more details.
// Implementation taken from github.com/mholt/acmez, which seems to be based on
// https://github.com/golang/crypto/blob/master/acme/jws.go.
func jwsSign(key crypto.Signer, hash crypto.Hash, digest []byte) ([]byte, error) {
	if key, ok := key.(*ecdsa.PrivateKey); ok {
		// The key.Sign method of ecdsa returns ASN1-encoded signature.
		// So, we use the package Sign function instead
		// to get R and S values directly and format the result accordingly.
		r, s, err := ecdsa.Sign(rand.Reader, key, digest)
		if err != nil {
			return nil, err
		}
		rb, sb := r.Bytes(), s.Bytes()
		size := key.Params().BitSize / 8
		if size%8 > 0 {
			size++
		}
		sig := make([]byte, size*2)
		copy(sig[size-len(rb):], rb)
		copy(sig[size*2-len(sb):], sb)
		return sig, nil
	}
	return key.Sign(rand.Reader, digest, hash)
}

// jwsHead constructs the protected JWS header for the given fields.
// Since jwk and kid are mutually-exclusive, the jwk will be encoded
// only if kid is empty. If nonce is empty, it will not be encoded.
// Implementation taken from github.com/mholt/acmez, which seems to be based on
// https://github.com/golang/crypto/blob/master/acme/jws.go.
func jwsHead(alg, nonce, u string, kid keyID, key crypto.Signer) (string, error) {
	phead := fmt.Sprintf(`{"alg":%q`, alg)
	if kid == noKeyID {
		jwk, err := jwkEncode(key.Public())
		if err != nil {
			return "", err
		}
		phead += fmt.Sprintf(`,"jwk":%s`, jwk)
	} else {
		phead += fmt.Sprintf(`,"kid":%q`, kid)
	}
	if nonce != "" {
		phead += fmt.Sprintf(`,"nonce":%q`, nonce)
	}
	phead += fmt.Sprintf(`,"url":%q}`, u)
	phead = base64.RawURLEncoding.EncodeToString([]byte(phead))
	return phead, nil
}

// jwkEncode encodes public part of an RSA or ECDSA key into a JWK.
// The result is also suitable for creating a JWK thumbprint.
// https://tools.ietf.org/html/rfc7517
// Implementation taken from github.com/mholt/acmez, which seems to be based on
// https://github.com/golang/crypto/blob/master/acme/jws.go.
func jwkEncode(pub crypto.PublicKey) (string, error) {
	switch pub := pub.(type) {
	case *rsa.PublicKey:
		// https://tools.ietf.org/html/rfc7518#section-6.3.1
		n := pub.N
		e := big.NewInt(int64(pub.E))
		// Field order is important.
		// See https://tools.ietf.org/html/rfc7638#section-3.3 for details.
		return fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`,
			base64.RawURLEncoding.EncodeToString(e.Bytes()),
			base64.RawURLEncoding.EncodeToString(n.Bytes()),
		), nil
	case *ecdsa.PublicKey:
		// https://tools.ietf.org/html/rfc7518#section-6.2.1
		p := pub.Curve.Params()
		n := p.BitSize / 8
		if p.BitSize%8 != 0 {
			n++
		}
		x := pub.X.Bytes()
		if n > len(x) {
			x = append(make([]byte, n-len(x)), x...)
		}
		y := pub.Y.Bytes()
		if n > len(y) {
			y = append(make([]byte, n-len(y)), y...)
		}
		// Field order is important.
		// See https://tools.ietf.org/html/rfc7638#section-3.3 for details.
		return fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`,
			p.Name,
			base64.RawURLEncoding.EncodeToString(x),
			base64.RawURLEncoding.EncodeToString(y),
		), nil
	}
	return "", errUnsupportedKey
}

// jwsFinal constructs the final JWS object.
// Implementation taken from github.com/mholt/acmez, which seems to be based on
// https://github.com/golang/crypto/blob/master/acme/jws.go.
func jwsFinal(sha crypto.Hash, sig []byte, phead, payload string) ([]byte, error) {
	enc := struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Sig       string `json:"signature"`
	}{
		Protected: phead,
		Payload:   payload,
		Sig:       base64.RawURLEncoding.EncodeToString(sig),
	}
	result, err := json.Marshal(&enc)
	if err != nil {
		return nil, err
	}
	return result, nil
}

type mockCA struct {
	MockIsRevoked func(sn string) (bool, error)
	MockRevoke    func(ctx context.Context, opts *authority.RevokeOptions) error
}

func (m *mockCA) Sign(cr *x509.CertificateRequest, opts provisioner.SignOptions, signOpts ...provisioner.SignOption) ([]*x509.Certificate, error) {
	return nil, nil
}

func (m *mockCA) IsRevoked(sn string) (bool, error) {
	if m.MockIsRevoked != nil {
		return m.MockIsRevoked(sn)
	}
	return false, nil
}

func (m *mockCA) Revoke(ctx context.Context, opts *authority.RevokeOptions) error {
	if m.MockRevoke != nil {
		return m.MockRevoke(ctx, opts)
	}
	return nil
}

func (m *mockCA) LoadProvisionerByName(string) (provisioner.Interface, error) {
	return nil, nil
}

func Test_validateReasonCode(t *testing.T) {
	tests := []struct {
		name       string
		reasonCode *int
		want       *acme.Error
	}{
		{
			name:       "ok",
			reasonCode: v(ocsp.Unspecified),
			want:       nil,
		},
		{
			name:       "fail/too-low",
			reasonCode: v(-1),
			want:       acme.NewError(acme.ErrorBadRevocationReasonType, "reasonCode out of bounds"),
		},
		{
			name:       "fail/too-high",
			reasonCode: v(11),
			want:       acme.NewError(acme.ErrorBadRevocationReasonType, "reasonCode out of bounds"),
		},
		{
			name:       "fail/missing-7",
			reasonCode: v(7),

			want: acme.NewError(acme.ErrorBadRevocationReasonType, "reasonCode out of bounds"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReasonCode(tt.reasonCode)
			if (err != nil) != (tt.want != nil) {
				t.Errorf("validateReasonCode() = %v, want %v", err, tt.want)
			}
			if err != nil {
				assert.Equals(t, err.Type, tt.want.Type)
				assert.Equals(t, err.Detail, tt.want.Detail)
				assert.Equals(t, err.Status, tt.want.Status)
				assert.Equals(t, err.Err.Error(), tt.want.Err.Error())
				assert.Equals(t, err.Detail, tt.want.Detail)
			}
		})
	}
}

func Test_reason(t *testing.T) {
	tests := []struct {
		name       string
		reasonCode int
		want       string
	}{
		{
			name:       "unspecified reason",
			reasonCode: ocsp.Unspecified,
			want:       "unspecified reason",
		},
		{
			name:       "key compromised",
			reasonCode: ocsp.KeyCompromise,
			want:       "key compromised",
		},
		{
			name:       "ca compromised",
			reasonCode: ocsp.CACompromise,
			want:       "ca compromised",
		},
		{
			name:       "affiliation changed",
			reasonCode: ocsp.AffiliationChanged,
			want:       "affiliation changed",
		},
		{
			name:       "superseded",
			reasonCode: ocsp.Superseded,
			want:       "superseded",
		},
		{
			name:       "cessation of operation",
			reasonCode: ocsp.CessationOfOperation,
			want:       "cessation of operation",
		},
		{
			name:       "certificate hold",
			reasonCode: ocsp.CertificateHold,
			want:       "certificate hold",
		},
		{
			name:       "remove from crl",
			reasonCode: ocsp.RemoveFromCRL,
			want:       "remove from crl",
		},
		{
			name:       "privilege withdrawn",
			reasonCode: ocsp.PrivilegeWithdrawn,
			want:       "privilege withdrawn",
		},
		{
			name:       "aa compromised",
			reasonCode: ocsp.AACompromise,
			want:       "aa compromised",
		},
		{
			name:       "default",
			reasonCode: -1,
			want:       "unspecified reason",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reason(tt.reasonCode); got != tt.want {
				t.Errorf("reason() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_revokeOptions(t *testing.T) {
	cert, _, err := generateCertKeyPair()
	assert.FatalError(t, err)
	type args struct {
		serial          string
		certToBeRevoked *x509.Certificate
		reasonCode      *int
	}
	tests := []struct {
		name string
		args args
		want *authority.RevokeOptions
	}{
		{
			name: "ok/no-reasoncode",
			args: args{
				serial:          "1234",
				certToBeRevoked: cert,
			},
			want: &authority.RevokeOptions{
				Serial: "1234",
				Crt:    cert,
				ACME:   true,
			},
		},
		{
			name: "ok/including-reasoncode",
			args: args{
				serial:          "1234",
				certToBeRevoked: cert,
				reasonCode:      v(ocsp.KeyCompromise),
			},
			want: &authority.RevokeOptions{
				Serial:     "1234",
				Crt:        cert,
				ACME:       true,
				ReasonCode: 1,
				Reason:     "key compromised",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := revokeOptions(tt.args.serial, tt.args.certToBeRevoked, tt.args.reasonCode); !cmp.Equal(got, tt.want) {
				t.Errorf("revokeOptions() diff = %s", cmp.Diff(got, tt.want))
			}
		})
	}
}

func TestHandler_RevokeCert(t *testing.T) {
	prov := &provisioner.ACME{
		Type: "ACME",
		Name: "testprov",
	}
	escProvName := url.PathEscape(prov.GetName())
	baseURL := &url.URL{Scheme: "https", Host: "test.ca.smallstep.com"}

	chiCtx := chi.NewRouteContext()
	revokeURL := fmt.Sprintf("%s/acme/%s/revoke-cert", baseURL.String(), escProvName)

	cert, key, err := generateCertKeyPair()
	assert.FatalError(t, err)
	rp := &revokePayload{
		Certificate: base64.RawURLEncoding.EncodeToString(cert.Raw),
	}
	payloadBytes, err := json.Marshal(rp)
	assert.FatalError(t, err)

	type test struct {
		db         acme.DB
		ca         acme.CertificateAuthority
		ctx        context.Context
		statusCode int
		err        *acme.Error
	}

	var tests = map[string]func(t *testing.T) test{
		"fail/no-jws": func(t *testing.T) test {
			ctx := context.Background()
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("jws expected in request context"),
			}
		},
		"fail/nil-jws": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), jwsContextKey, nil)
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("jws expected in request context"),
			}
		},
		"fail/no-provisioner": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), jwsContextKey, jws)
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("provisioner does not exist"),
			}
		},
		"fail/nil-provisioner": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), jwsContextKey, jws)
			ctx = context.WithValue(ctx, provisionerContextKey, nil)
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("provisioner does not exist"),
			}
		},
		"fail/no-payload": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), jwsContextKey, jws)
			ctx = context.WithValue(ctx, provisionerContextKey, prov)
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("payload does not exist"),
			}
		},
		"fail/nil-payload": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), jwsContextKey, jws)
			ctx = context.WithValue(ctx, provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, nil)
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("payload does not exist"),
			}
		},
		"fail/unmarshal-payload": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			malformedPayload := []byte(`{"payload":malformed?}`)
			ctx := context.WithValue(context.Background(), jwsContextKey, jws)
			ctx = context.WithValue(ctx, provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: malformedPayload})
			return test{
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("error unmarshaling payload"),
			}
		},
		"fail/wrong-certificate-encoding": func(t *testing.T) test {
			rp := &revokePayload{
				Certificate: base64.StdEncoding.EncodeToString(cert.Raw),
			}
			wronglyEncodedPayloadBytes, err := json.Marshal(rp)
			assert.FatalError(t, err)
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: wronglyEncodedPayloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			return test{
				ctx:        ctx,
				statusCode: 400,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:malformed",
					Status: 400,
					Detail: "The request message was malformed",
				},
			}
		},
		"fail/no-certificate-encoded": func(t *testing.T) test {
			rp := &revokePayload{
				Certificate: base64.RawURLEncoding.EncodeToString([]byte{}),
			}
			wrongPayloadBytes, err := json.Marshal(rp)
			assert.FatalError(t, err)
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: wrongPayloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			return test{
				ctx:        ctx,
				statusCode: 400,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:malformed",
					Status: 400,
					Detail: "The request message was malformed",
				},
			}
		},
		"fail/db.GetCertificateBySerial": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					return nil, errors.New("force")
				},
			}
			return test{
				db:         db,
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("error retrieving certificate by serial"),
			}
		},
		"fail/no-account": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{}, nil
				},
			}
			return test{
				db:         db,
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account not in context"),
			}
		},
		"fail/nil-account": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, accContextKey, nil)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{}, nil
				},
			}
			return test{
				db:         db,
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account not in context"),
			}
		},
		"fail/account-not-valid": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusInvalid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 403,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:unauthorized",
					Detail: fmt.Sprintf("No authorization provided for name %s", cert.Subject.String()),
					Status: 403,
				},
			}
		},
		"fail/account-not-authorized": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "differentAccountID",
					}, nil
				},
			}
			ca := &mockCA{}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 403,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:unauthorized",
					Detail: fmt.Sprintf("No authorization provided for name %s", cert.Subject.String()),
					Status: 403,
				},
			}
		},
		"fail/unauthorized-certificate-key": func(t *testing.T) test {
			_, unauthorizedKey, err := generateCertKeyPair()
			assert.FatalError(t, err)
			rp := &revokePayload{
				Certificate: base64.RawURLEncoding.EncodeToString(cert.Raw),
				ReasonCode:  v(1),
			}
			jwsBytes, err := jwsEncodeJSON(rp, unauthorizedKey, "", "nonce", revokeURL)
			assert.FatalError(t, err)
			jws, err := jose.ParseJWS(string(jwsBytes))
			assert.FatalError(t, err)
			unauthorizedPayloadBytes, err := json.Marshal(rp)
			assert.FatalError(t, err)
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: unauthorizedPayloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{}
			acmeErr := acme.NewError(acme.ErrorUnauthorizedType, "verification of jws using certificate public key failed")
			acmeErr.Detail = "No authorization provided for name CN=Test ACME Revoke Certificate"
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 403,
				err:        acmeErr,
			}
		},
		"fail/certificate-revoked-check-fails": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{
				MockIsRevoked: func(sn string) (bool, error) {
					return false, errors.New("force")
				},
			}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 500,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:serverInternal",
					Detail: "The server experienced an internal error",
					Status: 500,
				},
			}
		},
		"fail/certificate-already-revoked": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{
				MockIsRevoked: func(sn string) (bool, error) {
					return true, nil
				},
			}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 400,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:alreadyRevoked",
					Detail: "Certificate already revoked",
					Status: 400,
				},
			}
		},
		"fail/invalid-reasoncode": func(t *testing.T) test {
			rp := &revokePayload{
				Certificate: base64.RawURLEncoding.EncodeToString(cert.Raw),
				ReasonCode:  v(7),
			}
			wrongReasonCodePayloadBytes, err := json.Marshal(rp)
			assert.FatalError(t, err)
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: wrongReasonCodePayloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{
				MockIsRevoked: func(sn string) (bool, error) {
					return false, nil
				},
			}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 400,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:badRevocationReason",
					Detail: "The revocation reason provided is not allowed by the server",
					Status: 400,
				},
			}
		},
		"fail/prov.AuthorizeRevoke": func(t *testing.T) test {
			assert.FatalError(t, err)
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			mockACMEProv := &acme.MockProvisioner{
				MauthorizeRevoke: func(ctx context.Context, token string) error {
					return errors.New("force")
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, mockACMEProv)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{
				MockIsRevoked: func(sn string) (bool, error) {
					return false, nil
				},
			}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 500,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:serverInternal",
					Detail: "The server experienced an internal error",
					Status: 500,
				},
			}
		},
		"fail/ca.Revoke": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{
				MockRevoke: func(ctx context.Context, opts *authority.RevokeOptions) error {
					return errors.New("force")
				},
			}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 500,
				err: &acme.Error{
					Type:   "urn:ietf:params:acme:error:serverInternal",
					Detail: "The server experienced an internal error",
					Status: 500,
				},
			}
		},
		"fail/ca.Revoke-already-revoked": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{
				MockIsRevoked: func(sn string) (bool, error) {
					return false, nil
				},
				MockRevoke: func(ctx context.Context, opts *authority.RevokeOptions) error {
					return fmt.Errorf("certificate with serial number '%s' is already revoked", cert.SerialNumber.String())
				},
			}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAlreadyRevokedType, "certificate with serial number '%s' is already revoked", cert.SerialNumber.String()),
			}
		},
		"ok/using-account-key": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": revokeURL,
							},
						},
					},
				},
			}
			acc := &acme.Account{ID: "accountID", Status: acme.StatusValid}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 200,
			}
		},
		"ok/using-certificate-key": func(t *testing.T) test {
			rp := &revokePayload{
				Certificate: base64.RawURLEncoding.EncodeToString(cert.Raw),
				ReasonCode:  v(1),
			}
			jwsBytes, err := jwsEncodeJSON(rp, key, "", "nonce", revokeURL)
			assert.FatalError(t, err)
			jws, err := jose.ParseJWS(string(jwsBytes))
			assert.FatalError(t, err)
			payloadBytes, err := json.Marshal(rp)
			assert.FatalError(t, err)
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{value: payloadBytes})
			ctx = context.WithValue(ctx, jwsContextKey, jws)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			db := &acme.MockDB{
				MockGetCertificateBySerial: func(ctx context.Context, serial string) (*acme.Certificate, error) {
					assert.Equals(t, cert.SerialNumber.String(), serial)
					return &acme.Certificate{
						AccountID: "accountID",
					}, nil
				},
			}
			ca := &mockCA{}
			return test{
				db:         db,
				ca:         ca,
				ctx:        ctx,
				statusCode: 200,
			}
		},
	}
	for name, setup := range tests {
		tc := setup(t)
		t.Run(name, func(t *testing.T) {
			h := &Handler{linker: NewLinker("dns", "acme"), db: tc.db, ca: tc.ca}
			req := httptest.NewRequest("POST", revokeURL, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.RevokeCert(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.err) {
				var ae acme.Error
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))

				assert.Equals(t, ae.Type, tc.err.Type)
				assert.Equals(t, ae.Detail, tc.err.Detail)
				assert.Equals(t, ae.Identifier, tc.err.Identifier)
				assert.Equals(t, ae.Subproblems, tc.err.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.True(t, bytes.Equal(bytes.TrimSpace(body), []byte{}))
				assert.Equals(t, int64(0), req.ContentLength)
				assert.Equals(t, []string{fmt.Sprintf("<%s/acme/%s/directory>;rel=\"index\"", baseURL.String(), escProvName)}, res.Header["Link"])
			}
		})
	}
}