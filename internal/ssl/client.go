// Package ssl uploads a server certificate to Tencent Cloud's certificate
// product, so that EdgeOne has something to bind by ID.
//
// It exists because EdgeOne cannot be given a certificate directly. The teo API
// has no upload action at all — its certificate configuration takes a CertId
// that comes from this product, and nothing else. A gateway that terminates TLS
// itself never needs this; the certificate lives in a file and nginx reads it.
// A gateway that puts EdgeOne in front does, because the certificate has to be
// somewhere EdgeOne can fetch it from.
//
// Only the one action is here. Deleting, listing and renewing certificates are
// the console's job: this gateway uploads what it was given and reports what
// EdgeOne says about it, and a delete path would only add a way to break a
// running site.
package ssl

import (
	"context"
	"errors"
	"strings"

	"w103d/gateway/internal/tencentcloud"
)

const (
	host    = "ssl.tencentcloudapi.com"
	version = "2019-12-05"
)

// Credentials, APIError and Rejected are the shared Tencent Cloud types, so a
// caller that already handles one product's failures handles every product's.
type Credentials = tencentcloud.Credentials

type APIError = tencentcloud.APIError

func Rejected(err error) bool { return tencentcloud.Rejected(err) }

func endpoint() tencentcloud.Endpoint {
	// The certificate product is global: its own documentation states that the
	// region parameter is not required for these actions, so the header is left
	// off entirely rather than filled with a guess.
	return tencentcloud.Endpoint{Product: "SSL 证书", Host: host, Service: "ssl", Version: version}
}

// maxUpload bounds the PEM pair. The shared client refuses any request over
// 64 KiB with a message about encoding, which would send an operator looking for
// a serialization bug; a certificate that large is a certificate problem, and
// saying so is worth the second check.
const maxUpload = 48 << 10

type Client struct{ api *tencentcloud.Client }

func New(c Credentials) *Client { return &Client{api: tencentcloud.New(c)} }

// Upload sends one server certificate and its private key, and returns the
// certificate ID EdgeOne binds by.
//
// Repeatable is false on purpose. With it true — the API's default — a second
// upload of the same certificate creates a second certificate, so a lost
// response followed by a retry would leave two copies in the account and no way
// to tell which one EdgeOne is serving. With it false the product recognises the
// duplicate, and the ID it answers with is the existing one.
func (c *Client) Upload(ctx context.Context, chain, key, alias string) (string, error) {
	if strings.TrimSpace(chain) == "" || strings.TrimSpace(key) == "" {
		return "", errors.New("SSL 证书内容或私钥为空")
	}
	if len(chain)+len(key) > maxUpload {
		return "", errors.New("SSL 证书内容过大，无法上传到腾讯云证书服务")
	}
	var out struct {
		CertificateID *string `json:"CertificateId"`
		RepeatCertID  *string `json:"RepeatCertId"`
	}
	repeatable := false
	if err := c.api.Call(ctx, endpoint(), "UploadCertificate", map[string]any{
		"CertificatePublicKey":  chain,
		"CertificatePrivateKey": key,
		"CertificateType":       "SVR",
		"Alias":                 alias,
		"Repeatable":            repeatable,
	}, &out); err != nil {
		return "", err
	}
	// Which of the two fields carries the answer depends on whether the product
	// treated this as new or as a duplicate, and the documentation does not say
	// that the other is null rather than absent. Both are read.
	for _, candidate := range []*string{out.CertificateID, out.RepeatCertID} {
		if candidate != nil && *candidate != "" {
			return *candidate, nil
		}
	}
	return "", errors.New("SSL 证书服务未返回证书 ID")
}
