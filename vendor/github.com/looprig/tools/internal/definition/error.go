package definition

// BuildError reports an invalid dependency captured by a standard tool
// definition before any invokable tool is exposed.
type BuildError struct {
	Definition string
	Dependency string
	Cause      error
}

func (err *BuildError) Error() string {
	message := "tools: " + err.Definition + " definition has invalid " + err.Dependency
	if err.Cause != nil {
		return message + ": " + err.Cause.Error()
	}
	return message
}

func (err *BuildError) Unwrap() error { return err.Cause }
