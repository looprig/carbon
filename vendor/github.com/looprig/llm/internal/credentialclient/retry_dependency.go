package credentialclient

// Keep the inference retry decorator in the vendored dependency closure so
// composition tests exercise the production retry implementation.
import "github.com/looprig/inference/retry"

var _ = retry.New
