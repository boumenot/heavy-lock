package attribution

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

func TestParseRepository(t *testing.T) {
	for value, want := range map[string]Repository{
		"boumenot/heavy-lock":              {Path: "boumenot/heavy-lock"},
		"GitHub.com/boumenot/heavy-lock":   {Host: "github.com", Path: "boumenot/heavy-lock"},
		"ghe.example:8443/org/repo.name_1": {Host: "ghe.example:8443", Path: "org/repo.name_1"},
	} {
		got, err := ParseRepository(value)
		if err != nil || got != want {
			t.Errorf("ParseRepository(%q) = %+v, %v; want %+v", value, got, err, want)
		}
	}
	for _, value := range []string{"", "heavy-lock", "a/b/c/d", "localhost/a/b", "a/..", "a/b c", "/a/b", "a/b/"} {
		if _, err := ParseRepository(value); err == nil {
			t.Errorf("ParseRepository(%q) succeeded", value)
		}
	}
}

func TestParseWorklog(t *testing.T) {
	for value, want := range map[string]string{
		"33":                               "33",
		"boumenot/heavy-lock#33":           "boumenot/heavy-lock#33",
		"github.com/boumenot/heavy-lock#7": "boumenot/heavy-lock#7",
	} {
		got, err := ParseWorklog(value)
		if err != nil || got.String() != want {
			t.Errorf("ParseWorklog(%q) = %v, %v; want %s", value, got, err, want)
		}
	}
	w, _ := ParseWorklog("github.com/boumenot/heavy-lock#7")
	if w.URL() != "https://github.com/boumenot/heavy-lock/issues/7" {
		t.Errorf("URL = %q", w.URL())
	}
	for _, value := range []string{"", "0", "-1", "x", "#3", "a/b#", "a/b#0", "a#3", "33#"} {
		if _, err := ParseWorklog(value); err == nil {
			t.Errorf("ParseWorklog(%q) succeeded", value)
		}
	}
}

func TestParseRemoteURL(t *testing.T) {
	gh := Repository{Host: "github.com", Path: "boumenot/heavy-lock"}
	for remote, want := range map[string]Repository{
		"https://github.com/boumenot/heavy-lock.git":       gh,
		"https://user@github.com/boumenot/heavy-lock/":     gh,
		"git@github.com:boumenot/heavy-lock.git":           gh,
		"ssh://git@github.com:22/boumenot/heavy-lock":      gh,
		"git+ssh://git@GitHub.com/boumenot/heavy-lock.git": gh,
		"https://ghe.example:8443/org/repo":                {Host: "ghe.example:8443", Path: "org/repo"},
	} {
		got, ok := ParseRemoteURL(remote)
		if !ok || got != want {
			t.Errorf("ParseRemoteURL(%q) = %+v, %v; want %+v", remote, got, ok, want)
		}
	}
	for _, remote := range []string{
		"", "/tmp/repo", "../repo", "file:///tmp/a/b", "github.com:a/b",
		"https://github.com/a", "https://github.com/a/b/c", "https://localhost/a/b",
		"https://github.com/a/b?x=1", "./dir@host:a/b",
	} {
		if got, ok := ParseRemoteURL(remote); ok {
			t.Errorf("ParseRemoteURL(%q) = %+v", remote, got)
		}
	}
}

func fakeGit(responses map[string]string) Git {
	return func(_ context.Context, dir string, args ...string) (string, error) {
		if dir != "/work" {
			return "", errors.New("wrong dir")
		}
		out, ok := responses[strings.Join(args, " ")]
		if !ok {
			return "", errors.New("exit status 1")
		}
		return out, nil
	}
}

func TestDetectCheckout(t *testing.T) {
	commit := strings.Repeat("a", 40)
	got := DetectCheckout(context.Background(), "/work", fakeGit(map[string]string{
		"rev-parse --verify --quiet HEAD^{commit}": commit,
		"symbolic-ref --quiet --short HEAD":        "feature",
		"config --get branch.feature.remote":       "upstream",
		"remote get-url -- upstream":               "git@github.com:boumenot/heavy-lock.git",
		"remote get-url -- origin":                 "git@github.com:fork/heavy-lock.git",
	}))
	want := Checkout{Repository: Repository{Host: "github.com", Path: "boumenot/heavy-lock"}, Branch: "feature", Commit: commit}
	if got != want {
		t.Errorf("DetectCheckout = %+v; want %+v", got, want)
	}

	got = DetectCheckout(context.Background(), "/work", fakeGit(map[string]string{
		"rev-parse --verify --quiet HEAD^{commit}": commit,
		"remote get-url -- origin":                 "git@github.com:fork/heavy-lock.git",
	}))
	if got.Branch != "" || got.Repository.Path != "fork/heavy-lock" {
		t.Errorf("detached DetectCheckout = %+v", got)
	}
	if got := DetectCheckout(context.Background(), "/elsewhere", fakeGit(nil)); got != (Checkout{}) {
		t.Errorf("non-checkout DetectCheckout = %+v", got)
	}
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestBuild(t *testing.T) {
	checkout := Checkout{Repository: Repository{Host: "github.com", Path: "boumenot/heavy-lock"}, Branch: "main", Commit: strings.Repeat("b", 40)}
	worklog := Worklog{Number: 33}

	a := Build(Options{Worklog: &worklog}, checkout, env(map[string]string{
		"COPILOT_AGENT_SESSION_ID": "22caf59c-ccac-4db8-96ac-9723739b61fb",
		"AI_AGENT":                 CopilotAppAgent,
	}))
	if a.GetRepository() != "boumenot/heavy-lock" || a.GetBranch() != "main" || a.GetCommitSha() != checkout.Commit ||
		a.GetWorklog() != "boumenot/heavy-lock#33" || a.GetAgentSession() != "22caf59c-ccac-4db8-96ac-9723739b61fb" ||
		a.GetExtra()[RepositoryHostKey] != "github.com" || a.GetExtra()[WorklogURLKey] != "https://github.com/boumenot/heavy-lock/issues/33" ||
		a.GetExtra()[AIAgentKey] != CopilotAppAgent {
		t.Fatalf("Build = %v", a)
	}
	if got := SessionLink(a); got != "ghapp://sessions/22caf59c-ccac-4db8-96ac-9723739b61fb" {
		t.Errorf("SessionLink = %q", got)
	}

	// An explicit other repository drops the checkout's branch and commit.
	other := Repository{Path: "boumenot/other"}
	a = Build(Options{Repository: &other, Worklog: &worklog}, checkout, env(nil))
	if a.GetRepository() != "boumenot/other" || a.Branch != nil || a.CommitSha != nil ||
		a.GetWorklog() != "boumenot/other#33" || a.GetExtra()[WorklogURLKey] != "" || a.GetExtra()[RepositoryHostKey] != "" {
		t.Errorf("other-repository Build = %v", a)
	}

	// An explicit same repository keeps the detected host and checkout.
	same := Repository{Path: "Boumenot/Heavy-Lock"}
	a = Build(Options{Repository: &same}, checkout, env(nil))
	if a.GetBranch() != "main" || a.GetExtra()[RepositoryHostKey] != "github.com" {
		t.Errorf("same-repository Build = %v", a)
	}

	// A qualified worklog in another repository is kept as given.
	qualified := Worklog{Repository: Repository{Host: "ghe.example", Path: "org/plans"}, Number: 4}
	a = Build(Options{Worklog: &qualified}, checkout, env(nil))
	if a.GetWorklog() != "org/plans#4" || a.GetExtra()[WorklogURLKey] != "https://ghe.example/org/plans/issues/4" || a.GetRepository() != "boumenot/heavy-lock" {
		t.Errorf("qualified Build = %v", a)
	}

	// Without a checkout, an unqualified worklog stays a bare number.
	a = Build(Options{Worklog: &worklog}, Checkout{}, env(map[string]string{"COPILOT_AGENT_SESSION_ID": "bad id"}))
	if a.GetWorklog() != "33" || a.Repository != nil || a.AgentSession != nil || a.Extra != nil {
		t.Errorf("unqualified Build = %v", a)
	}
	if a := Build(Options{}, Checkout{}, env(nil)); a != nil {
		t.Errorf("empty Build = %v", a)
	}

	// A session from another agent runtime is recorded without an app link.
	a = Build(Options{}, Checkout{}, env(map[string]string{"COPILOT_AGENT_SESSION_ID": "abc", "AI_AGENT": "other"}))
	if a.GetAgentSession() != "abc" || SessionLink(a) != "" {
		t.Errorf("other-agent Build = %v, link %q", a, SessionLink(a))
	}
}

func TestLinkWorklog(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		a    *pb.Attribution
		want WorklogLink
	}{
		{"recorded", &pb.Attribution{Worklog: str("boumenot/heavy-lock#33"), Repository: str("boumenot/heavy-lock"),
			Extra: map[string]string{RepositoryHostKey: "github.com", WorklogURLKey: "https://github.com/boumenot/heavy-lock/issues/33"}},
			WorklogLink{Label: "boumenot/heavy-lock#33", URL: "https://github.com/boumenot/heavy-lock/issues/33", Repo: "github.com/boumenot/heavy-lock", Number: 33}},
		{"legacy with repository", &pb.Attribution{Worklog: str("16"), Repository: str("boumenot/heavy-lock"), Extra: map[string]string{RepositoryHostKey: "github.com"}},
			WorklogLink{Label: "boumenot/heavy-lock#16", URL: "https://github.com/boumenot/heavy-lock/issues/16", Repo: "github.com/boumenot/heavy-lock", Number: 16}},
		{"legacy bare", &pb.Attribution{Worklog: str("16")}, WorklogLink{Label: "16", Number: 16}},
		{"free text", &pb.Attribution{Worklog: str("plan A")}, WorklogLink{Label: "plan A"}},
		{"unsafe url", &pb.Attribution{Worklog: str("a/b#1"), Extra: map[string]string{WorklogURLKey: "javascript:alert(1)"}},
			WorklogLink{Label: "a/b#1", Repo: "a/b", Number: 1}},
	} {
		got, ok := LinkWorklog(tc.a)
		if !ok || got != tc.want {
			t.Errorf("%s: LinkWorklog = %+v, %v; want %+v", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := LinkWorklog(&pb.Attribution{}); ok {
		t.Error("LinkWorklog without worklog succeeded")
	}
}

func TestClientText(t *testing.T) {
	name, client, version := "tuna", "2.0.0-dev", "26.1"
	got := ClientText(&pb.ClientInfo{Name: &name, Version: &client, Os: pb.ClientOS_CLIENT_OS_MACOS,
		OsVersion: &version, Architecture: pb.ClientArchitecture_CLIENT_ARCHITECTURE_ARM64})
	if got != "tuna 2.0.0-dev · macOS 26.1 · arm64" {
		t.Errorf("ClientText = %q", got)
	}
	if ClientOS("darwin") != pb.ClientOS_CLIENT_OS_MACOS || ClientOS("plan9") != pb.ClientOS_CLIENT_OS_OTHER {
		t.Error("ClientOS mapping")
	}
}

func TestOSVersion(t *testing.T) {
	got := OSVersion()
	if runtime.GOOS == "darwin" && (got == "" || strings.ContainsAny(got, " \n")) {
		t.Errorf("OSVersion = %q", got)
	}
}
