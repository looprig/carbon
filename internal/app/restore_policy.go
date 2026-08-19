package app

import "github.com/looprig/harness/pkg/rig"

func carbonRestoreFailureOptions() []rig.RestoreFailureOption {
	return []rig.RestoreFailureOption{
		rig.AllowExternalCapabilityDrift(),
		rig.AllowRuntimeSkillsDrift(),
		rig.AllowConfinementDrift(),
		rig.AllowNativePermissionDrift(),
		rig.AllowPermissionPostureDrift(),
		rig.AllowRuntimeProfileDrift(),
		rig.AllowRuntimeCatalogDrift(),
		rig.AllowModelDrift(),
		rig.AllowEffortDrift(),
	}
}
