package blob

import "net/http"

// S3PartSize lets tests upload in small parts.
var S3PartSize = &s3PartSize

// S3BaseURL is where an S3 store's keys hang.
func S3BaseURL(s *Store) string { return s.be.(*s3Live).cur.Load().base.String() }

// S3MaxConns is the connection bound of an S3 store's transport.
func S3MaxConns(s *Store) (perHost, idle int) {
	tr := s.be.(*s3Live).cur.Load().client.Transport.(*http.Transport)
	return tr.MaxConnsPerHost, tr.MaxIdleConnsPerHost
}

// Rename replaces the rename a directory store commits with.
var Rename = &rename
