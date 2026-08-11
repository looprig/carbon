package loop

// CredentialMode identifies who supplies the credential used by a child
// harness: the product gateway or the harness's own login state.
type CredentialMode string

const (
	// CredentialGatewayBacked routes the child through the product gateway.
	CredentialGatewayBacked CredentialMode = "gateway-backed"
	// CredentialNativeAuth leaves authentication to the child harness.
	CredentialNativeAuth CredentialMode = "native-auth"
)
