// Package tencentcloud signs and sends Tencent Cloud API calls.
//
// Three products are used by this gateway — DNSPod for DNS records, EdgeOne for
// acceleration domains, and the SSL certificate product for the certificate
// EdgeOne binds — and all three share one signature scheme. The signer lives
// here rather than inside any one client so the three cannot drift apart in how
// they sign: a signature bug is then provable once instead of three times, and
// the two clients that were written later inherit the one that was already
// verified against the live API.
//
// Nothing in this package retries. A retried write against a cloud API whose
// first response was lost is how a record gets created twice.
package tencentcloud

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const contentType = "application/json; charset=utf-8"

// Credentials are one Tencent Cloud API key pair. The same type serves every
// product on purpose: which products a key may touch is a CAM policy, not a
// property of the key, so the gateway cannot know it locally and must not
// pretend to. What it can do is refuse a key that is obviously not one.
type Credentials struct {
	SecretID  string `json:"secretId"`
	SecretKey string `json:"secretKey"`
}

var credentialPattern = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{8,128}$`)

func (c Credentials) Valid() bool {
	return credentialPattern.MatchString(c.SecretID) && credentialPattern.MatchString(c.SecretKey)
}

// Hint is the masked form shown in the interface. It is the only piece of a
// credential that ever leaves the device.
func (c Credentials) Hint() string {
	if len(c.SecretID) <= 4 {
		return "••••"
	}
	return "••••" + c.SecretID[len(c.SecretID)-4:]
}

// APIError deliberately discards the upstream message, which may echo input back.
type APIError struct{ Code string }

func (e *APIError) Error() string { return "cloud request rejected (" + e.Code + ")" }

// Rejected proves that a write was rejected before mutation. Internal failures,
// transport errors and malformed responses leave its outcome unknown, and the
// two must never be treated alike: a rejected write is safe to correct and
// resend, an unknown one has to be read back first.
func Rejected(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	for _, prefix := range rejectedPrefixes {
		if e.Code == prefix || strings.HasPrefix(e.Code, prefix+".") {
			return true
		}
	}
	for _, code := range rejectedCodes {
		if e.Code == code {
			return true
		}
	}
	return false
}

// Every code under these prefixes is a refusal: the request was understood, the
// API declined it, and nothing was written.
var rejectedPrefixes = []string{
	"AuthFailure", "InvalidParameter", "MissingParameter", "UnauthorizedOperation",
	"LimitExceeded", "RequestLimitExceeded", "ResourceNotFound", "UnsupportedOperation",
}

// FailedOperation is deliberately NOT among them. It is the API's generic
// failure code, and a generic failure may have happened after a partial write —
// so treating all of it as a rejection would license resending a request whose
// first attempt already changed something. Only the sub-codes this gateway has
// actually seen reported before any mutation are listed here, each with the
// reason it is safe.
var rejectedCodes = []string{
	// Raised by the SSL product when a certificate delete is refused because
	// something still references it: the delete did not happen.
	"FailedOperation.BoundResources",
}

// Endpoint is one product's coordinates. Signing covers the host and the
// service name, so a client that sent DNSPod's signature to EdgeOne would be
// rejected by the API rather than silently accepted — which is exactly why the
// host is never a variable of the caller.
type Endpoint struct {
	// Product names the product in operator-facing error text.
	Product string
	// Host is the API host, e.g. "teo.tencentcloudapi.com".
	Host string
	// Service is the signing scope name, e.g. "teo".
	Service string
	// Version is the X-TC-Version header, e.g. "2022-09-01".
	Version string
	// Region is optional; empty omits the header entirely.
	Region string
}

type Client struct {
	credentials Credentials
	http        *http.Client
}

func New(c Credentials) *Client {
	return &Client{credentials: c, http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// WithTransport replaces the transport, returning the client for chaining. It
// exists so a caller can supply its own — a proxy, a pinned root — and so that
// the tests can send a signed request to a local server and read back exactly
// what was signed. The signature is computed from the endpoint, not from the
// transport, so swapping the transport cannot change what a request claims to
// be; it only changes where it goes, which is precisely what a test needs to
// control.
func (c *Client) WithTransport(rt http.RoundTripper) *Client {
	c.http.Transport = rt
	return c
}

func (c *Client) Credentials() Credentials { return c.credentials }

func shaHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func mac(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(message))
	return h.Sum(nil)
}

// The canonical request and key derivation follow Tencent Cloud's official TC3
// implementation (tencentcloud-sdk-go/tencentcloud/common/client.go).
func authorization(c Credentials, host, service string, timestamp int64, payload []byte) string {
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")
	scope := date + "/" + service + "/tc3_request"
	canonical := "POST\n/\n\ncontent-type:" + contentType + "\nhost:" + host + "\n\ncontent-type;host\n" + shaHex(payload)
	toSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(timestamp, 10) + "\n" + scope + "\n" + shaHex([]byte(canonical))
	key := mac(mac(mac([]byte("TC3"+c.SecretKey), date), service), "tc3_request")
	return "TC3-HMAC-SHA256 Credential=" + c.SecretID + "/" + scope + ", SignedHeaders=content-type;host, Signature=" + hex.EncodeToString(mac(key, toSign))
}

var errorCodePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]{0,100}$`)

// maxResponse bounds every reply. A cloud API that answers with an unbounded
// body is a memory exhaustion path into a device with 1.6 GiB of it.
const maxResponse = 4 << 20

// Call performs exactly one action and decodes exactly one response. Retries are
// deliberately absent; see the package comment.
func (c *Client) Call(ctx context.Context, ep Endpoint, action string, input, output any) error {
	if !c.credentials.Valid() {
		return fmt.Errorf("%s credentials are invalid", ep.Product)
	}
	payload, err := json.Marshal(input)
	if err != nil || len(payload) > 64<<10 {
		return fmt.Errorf("%s request cannot be encoded", ep.Product)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+ep.Host+"/", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%s request cannot be created", ep.Product)
	}
	timestamp := time.Now().Unix()
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("X-TC-Action", action)
	r.Header.Set("X-TC-Version", ep.Version)
	r.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	if ep.Region != "" {
		r.Header.Set("X-TC-Region", ep.Region)
	}
	r.Header.Set("Authorization", authorization(c.credentials, ep.Host, ep.Service, timestamp, payload))
	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("%s connection failed or timed out", ep.Product)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned an unexpected HTTP status", ep.Product)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(b) > maxResponse {
		return fmt.Errorf("%s response is incomplete or too large", ep.Product)
	}
	var envelope struct{ Response json.RawMessage }
	if json.Unmarshal(b, &envelope) != nil || len(envelope.Response) == 0 || bytes.Equal(envelope.Response, []byte("null")) {
		return fmt.Errorf("%s response is invalid", ep.Product)
	}
	var status struct {
		Error     *struct{ Code string }
		RequestID string `json:"RequestId"`
	}
	if json.Unmarshal(envelope.Response, &status) != nil {
		return fmt.Errorf("%s response is invalid", ep.Product)
	}
	if status.Error != nil {
		code := status.Error.Code
		if !errorCodePattern.MatchString(code) {
			code = "UnknownError"
		}
		return &APIError{Code: code}
	}
	if status.RequestID == "" || json.Unmarshal(envelope.Response, output) != nil {
		return fmt.Errorf("%s response is incomplete", ep.Product)
	}
	return nil
}
