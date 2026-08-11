package skill

import (
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

const (
	// Discovery reads an untrusted directory in fixed-size batches and stops at
	// explicit aggregate ceilings. One entry beyond the candidate ceiling is
	// read solely to detect overflow and fail the catalog closed.
	workspaceSkillReadDirBatch  = 32
	maxWorkspaceSkillCandidates = 256
	maxWorkspaceSkillDocuments  = 64
	maxWorkspaceSkillResults    = 32
)

// DiscoverWorkspaceSkills returns validated metadata for direct
// .skills/<name>/SKILL.md candidates beneath root. Discovery is intentionally
// metadata-only and fail-soft: malformed, unreadable, empty, mismatched, or
// unsafe candidates are omitted, and filesystem errors are not exposed to the
// caller. It reads directory entries in batches of 32 and reads at most 257
// entries: a directory exceeding 256 entries produces an empty catalog. For a
// bounded directory, eligible names are sorted before at most 64 documents are
// inspected and at most 32 records returned. Skill bodies remain available only
// through the gated Skill tool.
func DiscoverWorkspaceSkills(root string) []SkillMeta {
	return discoverWorkspaceSkills(root, nil)
}

// workspaceDiscoveryStats makes each aggregate ceiling directly observable to
// focused package tests without widening the public API.
type workspaceDiscoveryStats struct {
	readBatches int
	maxBatch    int
	candidates  int
	documents   int
	results     int
	oversized   bool
}

func discoverWorkspaceSkills(root string, stats *workspaceDiscoveryStats) []SkillMeta {
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer func() { _ = osRoot.Close() }()

	info, err := osRoot.Lstat(workspaceSkillsDir)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil
	}
	skillsRoot, err := osRoot.OpenRoot(workspaceSkillsDir)
	if err != nil {
		return nil
	}
	defer func() { _ = skillsRoot.Close() }()
	openedInfo, err := skillsRoot.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil
	}

	dir, err := skillsRoot.Open(".")
	if err != nil {
		return nil
	}
	defer func() { _ = dir.Close() }()

	candidateNames := make([]string, 0, maxWorkspaceSkillCandidates)
	candidates := 0
	for candidates <= maxWorkspaceSkillCandidates {
		batchSize := workspaceSkillReadDirBatch
		if remaining := maxWorkspaceSkillCandidates + 1 - candidates; remaining < batchSize {
			batchSize = remaining
		}
		entries, readErr := dir.ReadDir(batchSize)
		if stats != nil {
			stats.readBatches++
			if len(entries) > stats.maxBatch {
				stats.maxBatch = len(entries)
			}
		}
		for _, entry := range entries {
			candidates++
			if stats != nil {
				stats.candidates = candidates
			}
			if candidates > maxWorkspaceSkillCandidates {
				if stats != nil {
					stats.oversized = true
				}
				return nil
			}

			name := entry.Name()
			// Validate before the candidate name is used to construct a path.
			if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() || validateSkillName(name) != nil {
				continue
			}
			candidateNames = append(candidateNames, name)
		}
		if readErr != nil {
			if readErr != io.EOF {
				return nil
			}
			break
		}
	}

	sort.Strings(candidateNames)
	metas := make([]SkillMeta, 0, maxWorkspaceSkillResults)
	seen := make(map[string]struct{}, maxWorkspaceSkillResults)
	documents := 0
	for _, name := range candidateNames {
		if documents >= maxWorkspaceSkillDocuments || len(metas) >= maxWorkspaceSkillResults {
			break
		}

		candidateInfo, err := skillsRoot.Lstat(name)
		if err != nil || candidateInfo.Mode()&fs.ModeSymlink != 0 || !candidateInfo.IsDir() {
			continue
		}
		candidateRoot, err := skillsRoot.OpenRoot(name)
		if err != nil {
			continue
		}
		openedCandidateInfo, statErr := candidateRoot.Stat(".")
		if statErr != nil || !os.SameFile(candidateInfo, openedCandidateInfo) {
			_ = candidateRoot.Close()
			continue
		}

		documents++
		if stats != nil {
			stats.documents = documents
		}
		var meta SkillMeta
		artifactPath := workspaceSkillsDir + "/" + name + "/" + skillFileName
		_, err = loadWorkspaceSkillAtRoot(candidateRoot, skillFileName, artifactPath, name, &meta)
		_ = candidateRoot.Close()
		if err != nil {
			continue
		}
		// The directory is the loadable canonical identity. Requiring the parsed
		// name to match avoids advertising an identity the Skill tool cannot load.
		if meta.Name == "" || meta.Description == "" || meta.Name != name {
			continue
		}
		if _, exists := seen[meta.Name]; exists {
			continue
		}
		seen[meta.Name] = struct{}{}
		metas = append(metas, SkillMeta{
			Name:        strings.Clone(meta.Name),
			Description: strings.Clone(meta.Description),
		})
		if stats != nil {
			stats.results = len(metas)
		}
	}

	return metas
}
