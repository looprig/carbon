package model

// Sampling is dialect-neutral sampling intent. Each Codec maps it to its wire mechanism; the
// dialect-specific validity rules (e.g. Anthropic's Temperature==1.0 for thinking) live in the
// codec, not here.
type Sampling struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string
	Effort      Effort
}

// Clone returns a deep copy: pointer and slice fields are duplicated so the result never aliases
// the receiver's state (reuses cloneFloat64Ptr/cloneIntPtr from model.go).
func (s Sampling) Clone() Sampling {
	out := s
	out.Temperature = cloneFloat64Ptr(s.Temperature)
	out.TopP = cloneFloat64Ptr(s.TopP)
	out.MaxTokens = cloneIntPtr(s.MaxTokens)
	if s.Stop != nil {
		out.Stop = append([]string(nil), s.Stop...)
	}
	return out
}
