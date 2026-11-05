package blob

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearS3Env(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_ROLE_SESSION_NAME", "AWS_ENDPOINT_URL_STS", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
		"AWS_EC2_METADATA_SERVICE_ENDPOINT", "AWS_EC2_METADATA_DISABLED"} {
		t.Setenv(k, "")
	}
}

const credJSON = `{"AccessKeyId":"AK","SecretAccessKey":"SK","Token":"ST","Expiration":"2030-01-02T03:04:05Z"}`

func wantCreds(t *testing.T, src *s3CredSource) {
	t.Helper()
	if src == nil {
		t.Fatal("no credential source")
	}
	c, err := src.get()
	if err != nil {
		t.Fatal(err)
	}
	if c.access != "AK" || c.secret != "SK" || c.session != "ST" || !c.expires.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("creds = %+v", c)
	}
}

func TestS3WebIdentity(t *testing.T) {
	clearS3Env(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("the-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("Action") != "AssumeRoleWithWebIdentity" || r.FormValue("RoleArn") != "arn:aws:iam::1:role/r" ||
			r.FormValue("WebIdentityToken") != "the-jwt" || r.FormValue("RoleSessionName") != "commandcenter" {
			t.Errorf("form = %v", r.Form)
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			w.Write([]byte(`<ErrorResponse><Error><Code>AccessDenied</Code><Message>token the-jwt</Message></Error></ErrorResponse>`))
			return
		}
		w.Write([]byte(`<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
<AccessKeyId>AK</AccessKeyId><SecretAccessKey>SK</SecretAccessKey><SessionToken>ST</SessionToken><Expiration>2030-01-02T03:04:05Z</Expiration>
</Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer srv.Close()
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", token)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::1:role/r")
	t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)
	wantCreds(t, ambientS3Creds(S3Config{Region: "eu-west-1"}, time.Now))
	status = http.StatusForbidden
	_, err := ambientS3Creds(S3Config{}, time.Now).get()
	if err == nil || !strings.Contains(err.Error(), "403 AccessDenied") || strings.Contains(err.Error(), "the-jwt") {
		t.Fatalf("err = %v", err)
	}
}

func TestS3ContainerCredentials(t *testing.T) {
	clearS3Env(t)
	auth := filepath.Join(t.TempDir(), "auth")
	if err := os.WriteFile(auth, []byte("pod-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "pod-token" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(credJSON))
	}))
	defer srv.Close()
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", srv.URL+"/creds")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", auth)
	wantCreds(t, ambientS3Creds(S3Config{}, time.Now))
}

func TestS3InstanceProfile(t *testing.T) {
	clearS3Env(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" && r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "":
			w.Write([]byte("sess"))
		case r.Header.Get("X-aws-ec2-metadata-token") != "sess":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			w.Write([]byte("the-role\n"))
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/the-role":
			w.Write([]byte(credJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", srv.URL)
	wantCreds(t, ambientS3Creds(S3Config{Region: "us-east-2"}, time.Now))
	// Only AWS's endpoints get them, and the switch turns the service off.
	if ambientS3Creds(S3Config{Endpoint: "https://minio.example.com"}, time.Now) != nil {
		t.Fatal("asked the metadata service for a non-AWS endpoint")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	if ambientS3Creds(S3Config{}, time.Now) != nil {
		t.Fatal("asked a disabled metadata service")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "")
	srv.Close()
	if ambientS3Creds(S3Config{}, time.Now) != nil {
		t.Fatal("a source without a metadata service")
	}
}

// Credentials are refreshed shortly before they expire, and those still valid
// carry on through an outage of the source.
func TestS3CredSourceRefresh(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	var fail error
	src := &s3CredSource{now: func() time.Time { return now }, fetch: func() (s3Creds, error) {
		calls++
		if fail != nil {
			return s3Creds{}, fail
		}
		return s3Creds{access: "AK", secret: "SK", expires: now.Add(time.Hour)}, nil
	}}
	get := func(wantCalls int) {
		t.Helper()
		if _, err := src.get(); err != nil || calls != wantCalls {
			t.Fatalf("err %v, %d fetches, want %d", err, calls, wantCalls)
		}
	}
	get(1)
	now = now.Add(50 * time.Minute)
	get(1)
	now = now.Add(6 * time.Minute) // inside the refresh margin
	get(2)
	fail = os.ErrDeadlineExceeded
	now = now.Add(56 * time.Minute) // again inside it: the fetch fails, the credentials still hold
	get(3)
	now = now.Add(5 * time.Minute) // expired
	if _, err := src.get(); err == nil {
		t.Fatal("expired credentials were used")
	}
}
