package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/repo"
)

// s3Error is an error answered with S3's error document: the code names
// what went wrong for the client (the AWS S3 API reference, "Error
// responses"), the message is ours.
type s3Error struct {
	status    int
	code, msg string
}

func (e *s3Error) Error() string { return e.code + ": " + e.msg }

func errf(status int, code, format string, args ...any) *s3Error {
	return &s3Error{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

var (
	errNoSuchBucket = errf(http.StatusNotFound, "NoSuchBucket", "the bucket does not exist")
	errNoSuchKey    = errf(http.StatusNotFound, "NoSuchKey", "the key does not exist")
	errNoSuchUpload = errf(http.StatusNotFound, "NoSuchUpload", "the multipart upload does not exist; it may have been completed, aborted or expired")
	errPrecondition = errf(http.StatusPreconditionFailed, "PreconditionFailed", "a precondition of the request does not hold")
	errNotModified  = errf(http.StatusNotModified, "NotModified", "not modified")
)

func notImplemented(what string) *s3Error {
	return errf(http.StatusNotImplemented, "NotImplemented", "%s is not implemented by Command Center", what)
}

// asS3Error maps err: an s3Error as it is, a repository error by its status.
func asS3Error(err error) *s3Error {
	var e *s3Error
	if errors.As(err, &e) {
		return e
	}
	status := repo.StatusOf(err)
	code := map[int]string{
		http.StatusBadRequest: "InvalidRequest", http.StatusForbidden: "AccessDenied", http.StatusNotFound: "NoSuchKey",
		http.StatusMethodNotAllowed: "MethodNotAllowed", http.StatusRequestTimeout: "RequestTimeout",
		http.StatusRequestEntityTooLarge: "EntityTooLarge", http.StatusServiceUnavailable: "ServiceUnavailable",
		http.StatusInsufficientStorage: "InsufficientStorage",
	}[status]
	if code == "" {
		status, code = http.StatusInternalServerError, "InternalError"
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		msg = "internal error"
	}
	return &s3Error{status: status, code: code, msg: msg}
}

// writeError answers err as S3's XML error document (headers only for HEAD).
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	doc, status := errorDoc(r, err)
	h := w.Header()
	h.Del("Content-Length")
	h.Del("ETag")
	h.Del("Last-Modified")
	if status == http.StatusNotModified || r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	h.Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	writeXMLBody(w, doc)
}

type errorXML struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string
	RequestID string `xml:"RequestId"`
}

// errorDoc is err's error document and status; a server error is logged
// and its message replaced.
func errorDoc(r *http.Request, err error) (errorXML, int) {
	e := asS3Error(err)
	id := r.Header.Get("X-Request-Id")
	if e.status >= 500 {
		slog.Error("request failed", "id", id, "method", r.Method, "path", logsafe.Path(r.URL.Path), "err", logsafe.Err(err))
		if e.status == http.StatusInternalServerError {
			e = &s3Error{status: e.status, code: e.code, msg: "internal error (request id " + id + ")"}
		}
	}
	return errorXML{Code: e.code, Message: e.msg, Resource: r.URL.Path, RequestID: id}, e.status
}

// keepAliveEvery is how often slow writes white space.
const keepAliveEvery = 2 * time.Second

// slow answers the result of fn, a step that may take long (assembling or
// copying a large object), as S3 answers CompleteMultipartUpload and
// CopyObject (AWS docs: "CompleteMultipartUpload"): 200 at once, then
// white space every keepAliveEvery, so that no client or proxy drops the
// silent connection (the AWS CRT client does after a few seconds), then
// fn's document, or the error document of its failure, which the SDKs
// look for in a 200 of these operations.
func slow(w http.ResponseWriter, r *http.Request, fn func() (any, error)) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, xml.Header)
	rc := http.NewResponseController(w)
	rc.Flush()
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(keepAliveEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				io.WriteString(w, " ")
				rc.Flush()
			}
		}
	}()
	v, err := fn()
	close(done)
	<-stopped
	if err != nil {
		v, _ = errorDoc(r, err)
	}
	xml.NewEncoder(w).Encode(v)
}

// writeXML answers 200 with v as an XML document.
func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	writeXMLBody(w, v)
}

func writeXMLBody(w http.ResponseWriter, v any) {
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}
