package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCILintBuildUsesScopedFormatCheck(t *testing.T) {
	workflow := readCIWorkflow(t)
	jobs := workflowJobs(t, workflow)
	lintBuild := jobs["lint-build"]
	formatStep := workflowStep(t, lintBuild, "gofmt")
	if !strings.Contains(formatStep, "run: make fmt-check") {
		t.Fatalf("lint-build gofmt step must invoke make fmt-check; step:\n%s", formatStep)
	}
	if strings.Contains(formatStep, "gofmt -l .") {
		t.Fatalf("lint-build gofmt step must not recursively inspect vendor; step:\n%s", formatStep)
	}

	for _, forbidden := range []string{
		"self-hosted",
		"windows-restricted",
		"windows-elevated",
		"sandbox-standard",
		"sandbox-elevated",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("CI workflow must not contain %q", forbidden)
		}
	}

	crossCompile := jobs["windows-cross-compile"]
	if !strings.Contains(crossCompile, "runs-on: ubuntu-latest") {
		t.Fatalf("windows-cross-compile must run on ubuntu-latest; job:\n%s", crossCompile)
	}
	for _, command := range []string{
		"CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath ./...",
		"CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./...",
		"CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -tags integration ./...",
		"CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath ./...",
		"CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -tags integration ./...",
	} {
		if !strings.Contains(crossCompile, command) {
			t.Fatalf("windows-cross-compile must retain %q; job:\n%s", command, crossCompile)
		}
	}

	if _, ok := jobs["test-linux"]; ok {
		t.Fatal("CI must not require live Linux sandbox enforcement on a generic hosted runner")
	}
	for _, unavailableSetup := range []string{
		"apparmor_restrict_unprivileged_userns",
		"unprivileged_userns_clone",
		"modprobe nf_conntrack",
		"modprobe nf_tables",
	} {
		if strings.Contains(workflow, unavailableSetup) {
			t.Fatalf("CI workflow must not contain hosted-kernel setup %q", unavailableSetup)
		}
	}

	assertNoDanglingNeeds(t, jobs)
}

func readCIWorkflow(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	workflowPath := filepath.Join(filepath.Dir(filename), "..", "..", ".github", "workflows", "ci.yml")
	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	return string(workflow)
}

func workflowJobs(t *testing.T, workflow string) map[string]string {
	t.Helper()

	lines := strings.Split(workflow, "\n")
	jobsLine := -1
	for i, line := range lines {
		if line == "jobs:" {
			jobsLine = i
			break
		}
	}
	if jobsLine < 0 {
		t.Fatal("CI workflow has no jobs block")
	}

	jobs := make(map[string]string)
	var name string
	var block strings.Builder
	flush := func() {
		if name != "" {
			jobs[name] = block.String()
		}
		name = ""
		block.Reset()
	}
	for _, line := range lines[jobsLine+1:] {
		if line != "" && !strings.HasPrefix(line, " ") {
			flush()
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(trimmed, ":") {
			flush()
			name = strings.TrimSuffix(trimmed, ":")
			continue
		}
		if name != "" {
			block.WriteString(line)
			block.WriteByte('\n')
		}
	}
	flush()
	return jobs
}

func workflowStep(t *testing.T, job, name string) string {
	t.Helper()

	prefix := "      - name: " + name
	lines := strings.Split(job, "\n")
	start := -1
	for i, line := range lines {
		if line == prefix {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("CI job has no %q step; job:\n%s", name, job)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "      - name: ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

func assertNoDanglingNeeds(t *testing.T, jobs map[string]string) {
	t.Helper()

	for jobName, job := range jobs {
		for _, line := range strings.Split(job, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "needs:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "needs:"))
			value = strings.Trim(value, "[]")
			for _, dependency := range strings.Split(value, ",") {
				dependency = strings.Trim(strings.TrimSpace(dependency), "\"'")
				if dependency != "" {
					if _, ok := jobs[dependency]; !ok {
						t.Errorf("job %q needs missing job %q", jobName, dependency)
					}
				}
			}
		}
	}
}
