package sessionruntime

const (
	sessionResourceWindowsObjectInheritACE    uint8 = 0x01
	sessionResourceWindowsContainerInheritACE uint8 = 0x02
)

func sessionResourceWindowsACEFlagsArePrivate(flags uint8, directory bool) bool {
	if directory {
		return flags == sessionResourceWindowsObjectInheritACE|sessionResourceWindowsContainerInheritACE
	}
	return flags == 0
}
