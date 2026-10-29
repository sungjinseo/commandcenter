package transfer

// SetAfterAsset replaces the hook called after each exported file and
// returns the restore.
func SetAfterAsset(f func(path string)) (restore func()) {
	old := afterAsset
	afterAsset = f
	return func() { afterAsset = old }
}
