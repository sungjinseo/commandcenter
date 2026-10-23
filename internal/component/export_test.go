package component

// Exported for parity_test.go (package component_test), which compares the
// private copies with golang.Unescape and pypi.Normalize directly.
var (
	GoUnescape    = goUnescape
	PyPINormalize = pypiNormalize
)
