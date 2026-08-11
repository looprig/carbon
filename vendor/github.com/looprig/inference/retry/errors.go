package retry

// InvalidResponseError reports an inner client that returned no response and
// no error. A retry decorator cannot classify or replay this malformed result.
type InvalidResponseError struct{}

func (*InvalidResponseError) Error() string {
	return "retry: inner client returned nil response without error"
}
