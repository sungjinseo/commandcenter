package blob

// S3PartSize lets tests upload in small parts.
var S3PartSize = &s3PartSize

// S3BaseURL is where an S3 store's keys hang.
func S3BaseURL(s *Store) string { return s.be.(*s3).base.String() }
