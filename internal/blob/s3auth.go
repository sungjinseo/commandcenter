package blob

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Temporary S3 credentials for a store with no keys of its own, in the AWS
// SDKs' order after the AWS_ACCESS_KEY_ID variables: a web identity token
// (EKS IRSA), the container credentials endpoint (EKS Pod Identity, ECS) and
// the instance metadata service (EC2 instance profile, IMDSv2). Errors never
// hold a token or key.

// s3Refresh is how long before they expire credentials are fetched again.
const s3Refresh = 5 * time.Minute

type s3Creds struct {
	access, secret, session string
	expires                 time.Time
}

// s3CredSource keeps what fetch gave until shortly before it expires.
type s3CredSource struct {
	fetch func() (s3Creds, error)
	now   func() time.Time
	mu    sync.Mutex
	cur   s3Creds
}

func (c *s3CredSource) get() (s3Creds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.cur.access != "" && now.Before(c.cur.expires.Add(-s3Refresh)) {
		return c.cur, nil
	}
	n, err := c.fetch()
	if err != nil {
		if c.cur.access != "" && now.Before(c.cur.expires) { // what it holds works through an outage of the source
			return c.cur, nil
		}
		return s3Creds{}, err
	}
	c.cur = n
	return n, nil
}

// ambientS3Creds is the credential source the environment offers for cfg, or
// nil. The metadata service is only asked for AWS's own endpoints: nothing else
// takes its credentials, and off EC2 the question costs a timeout.
func ambientS3Creds(cfg S3Config, now func() time.Time) *s3CredSource {
	src := &s3CredSource{now: now}
	metadata := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}} // link-local: never through a proxy
	switch {
	case os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" && os.Getenv("AWS_ROLE_ARN") != "":
		src.fetch = func() (s3Creds, error) { return stsWebIdentity(cfg.Region) }
		return src
	case os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" || os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "":
		src.fetch = func() (s3Creds, error) { return containerCreds(metadata) }
		return src
	}
	if os.Getenv("AWS_EC2_METADATA_DISABLED") == "true" || !strings.HasSuffix(cfg.endpointURL().Hostname(), ".amazonaws.com") {
		return nil
	}
	src.fetch = func() (s3Creds, error) { return instanceCreds(metadata) }
	if _, err := src.get(); err != nil { // no instance profile: as if no source
		return nil
	}
	return src
}

// stsWebIdentity exchanges the projected token, which the kubelet rotates, so
// it is read again each time.
func stsWebIdentity(region string) (s3Creds, error) {
	token, err := os.ReadFile(os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"))
	if err != nil {
		return s3Creds{}, fmt.Errorf("web identity token: %w", err)
	}
	endpoint := os.Getenv("AWS_ENDPOINT_URL_STS")
	if endpoint == "" {
		for _, r := range []string{os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), region} {
			if r != "" {
				endpoint = "https://sts." + r + ".amazonaws.com"
				break
			}
		}
	}
	session := os.Getenv("AWS_ROLE_SESSION_NAME")
	if session == "" {
		session = "commandcenter"
	}
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"}, "RoleArn": {os.Getenv("AWS_ROLE_ARN")},
		"RoleSessionName": {session}, "WebIdentityToken": {strings.TrimSpace(string(token))}}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(endpoint+"/", form)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) { // not the URL's form, which holds no secret here, but keep to the cause
			err = ue.Err
		}
		return s3Creds{}, fmt.Errorf("sts: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return s3Creds{}, fmt.Errorf("sts: %w", err)
	}
	var out struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string
			SessionToken    string
			Expiration      string
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
		Code string `xml:"Error>Code"`
	}
	xmlErr := xml.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		return s3Creds{}, fmt.Errorf("sts: HTTP %d %.100s", resp.StatusCode, out.Code)
	}
	if xmlErr != nil {
		return s3Creds{}, errors.New("sts: the answer is not XML")
	}
	c := out.Credentials
	return newS3Creds(c.AccessKeyID, c.SecretAccessKey, c.SessionToken, c.Expiration)
}

// containerCreds asks the endpoint EKS Pod Identity and ECS name.
func containerCreds(client *http.Client) (s3Creds, error) {
	uri := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if uri == "" {
		uri = "http://169.254.170.2" + os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	}
	auth := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
	if f := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return s3Creds{}, fmt.Errorf("container credentials token: %w", err)
		}
		auth = strings.TrimSpace(string(b))
	}
	hdr := http.Header{}
	if auth != "" {
		hdr.Set("Authorization", auth)
	}
	return jsonCreds(client, uri, hdr)
}

// instanceCreds asks the metadata service, by IMDSv2's session token.
func instanceCreds(client *http.Client) (s3Creds, error) {
	base := strings.TrimSuffix(os.Getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT"), "/")
	if base == "" {
		base = "http://169.254.169.254"
	}
	req, err := http.NewRequest(http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return s3Creds{}, err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
	token, err := metadataText(client, req)
	if err != nil {
		return s3Creds{}, err
	}
	const path = "/latest/meta-data/iam/security-credentials/"
	req, err = http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return s3Creds{}, err
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)
	role, err := metadataText(client, req)
	if err != nil {
		return s3Creds{}, err
	}
	role, _, _ = strings.Cut(strings.TrimSpace(role), "\n")
	return jsonCreds(client, base+path+url.PathEscape(role), http.Header{"X-Aws-Ec2-Metadata-Token": {token}})
}

func metadataText(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("the metadata service does not answer")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata service: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return string(b), err
}

func jsonCreds(client *http.Client, uri string, hdr http.Header) (s3Creds, error) {
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return s3Creds{}, errors.New("credentials endpoint: bad URL")
	}
	req.Header = hdr
	b, err := metadataText(client, req)
	if err != nil {
		return s3Creds{}, err
	}
	var c struct{ AccessKeyId, SecretAccessKey, Token, Expiration string }
	if json.Unmarshal([]byte(b), &c) != nil {
		return s3Creds{}, errors.New("credentials endpoint: the answer is not JSON")
	}
	return newS3Creds(c.AccessKeyId, c.SecretAccessKey, c.Token, c.Expiration)
}

func newS3Creds(access, secret, session, expiration string) (s3Creds, error) {
	if access == "" || secret == "" {
		return s3Creds{}, errors.New("the answer holds no credentials")
	}
	t, err := time.Parse(time.RFC3339, expiration)
	if err != nil {
		return s3Creds{}, errors.New("the answer has no valid expiration")
	}
	return s3Creds{access: access, secret: secret, session: session, expires: t}, nil
}
