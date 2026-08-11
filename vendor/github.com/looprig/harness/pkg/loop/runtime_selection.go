package loop

// RuntimeSourceName identifies the owner of a child runtime selection.
// It is deliberately narrower than CredentialMode: source is a stable
// catalogue identity, while credential mode describes the authentication
// contract used to launch the child.
type RuntimeSourceName string

const (
	RuntimeSourceGateway RuntimeSourceName = "gateway"
	RuntimeSourceNative  RuntimeSourceName = "native"
)

// RuntimeSelectionKind identifies whether the product composition root selected
// or delegated model selection to the child harness.
type RuntimeSelectionKind string

const (
	RuntimeSelectionExplicit       RuntimeSelectionKind = "explicit"
	RuntimeSelectionHarnessManaged RuntimeSelectionKind = "harness-managed"
)

func (s RuntimeSourceName) valid() bool {
	return s == RuntimeSourceGateway || s == RuntimeSourceNative
}

func (k RuntimeSelectionKind) valid() bool {
	return k == RuntimeSelectionExplicit || k == RuntimeSelectionHarnessManaged
}

func sourceForCredential(credential CredentialMode) RuntimeSourceName {
	switch credential {
	case CredentialGatewayBacked:
		return RuntimeSourceGateway
	case CredentialNativeAuth:
		return RuntimeSourceNative
	default:
		return ""
	}
}

func credentialForSource(source RuntimeSourceName) CredentialMode {
	switch source {
	case RuntimeSourceGateway:
		return CredentialGatewayBacked
	case RuntimeSourceNative:
		return CredentialNativeAuth
	default:
		return ""
	}
}
