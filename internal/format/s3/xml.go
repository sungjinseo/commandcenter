package s3

import (
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"commandcenter/internal/auth"
	"commandcenter/internal/logsafe"
	"commandcenter/internal/repo"
	"commandcenter/internal/sigv4"
)

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

// Error is an S3 error response: an HTTP status and an S3 error code.
type Error struct {
	Status int
	Code   string
	Msg    string
	Region string // AuthorizationHeaderMalformed: the region to sign for
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func s3err(status int, code, msg string) *Error { return &Error{Status: status, Code: code, Msg: msg} }

var (
	errNoSuchBucket   = s3err(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
	errNoSuchKey      = s3err(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
	errNoSuchUpload   = s3err(http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.")
	errAccessDenied   = s3err(http.StatusForbidden, "AccessDenied", "Access Denied")
	errPrecondition   = s3err(http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
	errBadDigest      = s3err(http.StatusBadRequest, "BadDigest", "The Content-MD5 or checksum you specified did not match what we received.")
	errNotImplemented = s3err(http.StatusNotImplemented, "NotImplemented", "A header or query you provided implies functionality that is not implemented.")
	errMalformedXML   = s3err(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
	errInvalidKey     = s3err(http.StatusBadRequest, "KeyTooLongError", "Your key is too long, empty, or holds characters or segments that are not allowed.")
)

func invalidArgument(msg string) *Error { return s3err(http.StatusBadRequest, "InvalidArgument", msg) }

// toS3 maps any error of a request to an S3 error.
func toS3(r *http.Request, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var th *auth.ThrottledError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &th):
		return s3err(http.StatusServiceUnavailable, "SlowDown", "Too many failed signatures for this access key; slow down.")
	case errors.Is(err, auth.ErrS3KeyUnknown):
		return s3err(http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
	case errors.Is(err, auth.ErrS3KeyExpired):
		return s3err(http.StatusBadRequest, "ExpiredToken", "The provided token has expired.")
	case errors.Is(err, auth.ErrBadCredentials):
		return s3err(http.StatusForbidden, "AccessDenied", "The owner of the access key is disabled or unknown.")
	case errors.Is(err, sigv4.ErrSignature), errors.Is(err, sigv4.ErrChunkSignature):
		return s3err(http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your key and signing method.")
	case errors.Is(err, sigv4.ErrSkewed):
		return s3err(http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.")
	case errors.Is(err, sigv4.ErrExpired):
		return s3err(http.StatusForbidden, "AccessDenied", "Request has expired")
	case errors.Is(err, sigv4.ErrV2):
		return s3err(http.StatusBadRequest, "InvalidRequest", "The authorization mechanism you have provided is not supported. Please use AWS4-HMAC-SHA256.")
	case errors.Is(err, sigv4.ErrMalformed):
		return s3err(http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header is malformed.")
	case errors.Is(err, sigv4.ErrTrailerChecksum):
		return errBadDigest
	case errors.Is(err, sigv4.ErrChunkFormat), errors.Is(err, sigv4.ErrDecodedLength):
		return s3err(http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.")
	case errors.As(err, &tooBig):
		return s3err(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size.")
	}
	status, msg := repo.ErrorMessage(r, err)
	switch status {
	case http.StatusNotFound:
		return errNoSuchKey
	case http.StatusForbidden, http.StatusUnauthorized:
		return s3err(http.StatusForbidden, "AccessDenied", msg)
	case http.StatusRequestEntityTooLarge:
		return s3err(http.StatusBadRequest, "EntityTooLarge", msg)
	case http.StatusRequestTimeout:
		return s3err(http.StatusBadRequest, "RequestTimeout", "Your socket connection to the server was not read from or written to within the timeout period.")
	case http.StatusInsufficientStorage:
		return s3err(http.StatusInsufficientStorage, "InsufficientStorage", msg)
	case http.StatusServiceUnavailable:
		return s3err(http.StatusServiceUnavailable, "ServiceUnavailable", msg)
	case http.StatusConflict:
		return s3err(http.StatusConflict, "OperationAborted", msg)
	case http.StatusMethodNotAllowed:
		return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", msg)
	}
	if status >= 500 {
		return s3err(http.StatusInternalServerError, "InternalError", msg)
	}
	return invalidArgument(msg)
}

type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Region    string   `xml:"Region,omitempty"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

// writeError answers err as S3 does: the XML error document, or only the
// status for a HEAD.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	e := toS3(r, err)
	if e.Status >= 500 && e.Code != "ServiceUnavailable" && e.Code != "SlowDown" && e.Code != "NotImplemented" {
		logFailure(r, err)
	}
	if e.Code == "SlowDown" || e.Code == "ServiceUnavailable" {
		w.Header().Set("Retry-After", "30")
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Amz-Request-Id", r.Header.Get("X-Request-Id"))
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	w.WriteHeader(e.Status)
	writeXMLBody(w, errBody(r, e))
}

func errBody(r *http.Request, e *Error) errorBody {
	return errorBody{Code: e.Code, Message: e.Msg, Region: e.Region, Resource: r.URL.Path, RequestID: r.Header.Get("X-Request-Id")}
}

// writeXML answers v with status 200.
func writeXML(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Amz-Request-Id", r.Header.Get("X-Request-Id"))
	w.WriteHeader(http.StatusOK)
	writeXMLBody(w, v)
}

func writeXMLBody(w http.ResponseWriter, v any) {
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}

// s3Time formats a time as S3's XML documents do.
func s3Time(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0)
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// quoteETag quotes an ETag value for a header or a document.
func quoteETag(e string) string { return `"` + strings.Trim(e, `"`) + `"` }

func logFailure(r *http.Request, err error) {
	slog.Error("s3 request failed", "id", r.Header.Get("X-Request-Id"), "method", r.Method,
		"path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
}
