// Package attribution derives job attribution at submit time and builds the
// links shown for stored attribution. Values come only from explicit options,
// the job's git checkout or variables set by the invoking agent runtime;
// anything that cannot be determined is left absent rather than guessed.
package attribution

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

// Extra metadata keys written by the runner.
const (
	// RepositoryHostKey names the host of Attribution.repository, e.g. github.com.
	RepositoryHostKey = "repository_host"
	// WorklogURLKey is the worklog issue's web URL, recorded when its host is known.
	WorklogURLKey = "worklog_url"
	// AIAgentKey records the invoking agent runtime's AI_AGENT value.
	AIAgentKey = "ai_agent"
)

// CopilotAppAgent is the AI_AGENT value set by the GitHub Copilot app, whose
// session IDs open with the documented ghapp://sessions/SESSION_ID link.
const CopilotAppAgent = "github_copilot_app_agent"

// Repository is a hosted repository. Host is empty when unknown.
type Repository struct {
	Host string
	Path string // OWNER/NAME
}

func (r Repository) String() string {
	if r.Host == "" {
		return r.Path
	}
	return r.Host + "/" + r.Path
}

// Worklog is a worklog issue. Repository.Path is empty when unqualified.
type Worklog struct {
	Repository Repository
	Number     uint64
}

var (
	segment     = `[A-Za-z0-9_.-]+`
	pathPattern = regexp.MustCompile(`^` + segment + `/` + segment + `$`)
	hostPattern = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?$`)
	sessionID   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	commitHash  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

func validPath(path string) bool {
	if !pathPattern.MatchString(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// ParseRepository accepts OWNER/NAME or HOST/OWNER/NAME.
func ParseRepository(value string) (Repository, error) {
	invalid := errors.New("repository must be OWNER/NAME or HOST/OWNER/NAME")
	parts := strings.Split(value, "/")
	var r Repository
	switch len(parts) {
	case 2:
		r.Path = value
	case 3:
		r.Host, r.Path = strings.ToLower(parts[0]), parts[1]+"/"+parts[2]
		if !hostPattern.MatchString(r.Host) || !strings.Contains(r.Host, ".") {
			return Repository{}, invalid
		}
	default:
		return Repository{}, invalid
	}
	if !validPath(r.Path) {
		return Repository{}, invalid
	}
	return r, nil
}

// ParseWorklog accepts N or [HOST/]OWNER/NAME#N with a positive N.
func ParseWorklog(value string) (Worklog, error) {
	invalid := errors.New("worklog must be a positive issue number N or [HOST/]OWNER/NAME#N")
	repository, number, qualified := strings.Cut(value, "#")
	if !qualified {
		number = value
	}
	n, err := strconv.ParseUint(number, 10, 63)
	if err != nil || n == 0 {
		return Worklog{}, invalid
	}
	w := Worklog{Number: n}
	if qualified {
		if w.Repository, err = ParseRepository(repository); err != nil {
			return Worklog{}, invalid
		}
	}
	return w, nil
}

// String is the stored form: N, or OWNER/NAME#N when the repository is known.
func (w Worklog) String() string {
	if w.Repository.Path == "" {
		return strconv.FormatUint(w.Number, 10)
	}
	return w.Repository.Path + "#" + strconv.FormatUint(w.Number, 10)
}

// URL is the issue's web URL, or "" when its host is unknown.
func (w Worklog) URL() string {
	if w.Repository.Host == "" || w.Repository.Path == "" {
		return ""
	}
	return (&url.URL{Scheme: "https", Host: w.Repository.Host,
		Path: "/" + w.Repository.Path + "/issues/" + strconv.FormatUint(w.Number, 10)}).String()
}

// ParseRemoteURL extracts the repository from a git remote URL:
// http(s)://HOST/OWNER/NAME, ssh://[USER@]HOST[:PORT]/OWNER/NAME or
// USER@HOST:OWNER/NAME, each optionally ending in .git or /.
func ParseRemoteURL(remote string) (Repository, bool) {
	var host, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return Repository{}, false
		}
		switch u.Scheme {
		case "https", "http":
			host = u.Host
		case "ssh", "git+ssh", "git":
			// SSH and git-protocol ports are not web ports.
			host = u.Hostname()
		default:
			return Repository{}, false
		}
		path = u.Path
	} else {
		// scp-like syntax requires a user, so local paths are never matched.
		at := strings.Index(remote, "@")
		colon := strings.Index(remote, ":")
		if at <= 0 || colon < at || strings.Contains(remote[:at], "/") {
			return Repository{}, false
		}
		host, path = remote[at+1:colon], remote[colon+1:]
	}
	path = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), ".git")
	host = strings.ToLower(host)
	if !hostPattern.MatchString(host) || !strings.Contains(host, ".") || !validPath(path) {
		return Repository{}, false
	}
	return Repository{Host: host, Path: path}, true
}

// Checkout is what git reports for a working directory; empty fields are unknown.
type Checkout struct {
	Repository Repository
	Branch     string
	Commit     string
}

// Git runs git with arguments in dir and returns trimmed standard output.
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// RunGit runs the git executable with env, each call bounded by a short timeout.
func RunGit(env []string) Git {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
}

// DetectCheckout reads the commit, branch and repository of dir's checkout.
// The repository comes from the current branch's upstream remote, else origin.
func DetectCheckout(ctx context.Context, dir string, git Git) Checkout {
	var c Checkout
	if out, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil && commitHash.MatchString(out) {
		c.Commit = out
	}
	if out, err := git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && out != "" {
		c.Branch = out
	}
	remote := "origin"
	if c.Branch != "" {
		if out, err := git(ctx, dir, "config", "--get", "branch."+c.Branch+".remote"); err == nil && out != "" && out != "." {
			remote = out
		}
	}
	if out, err := git(ctx, dir, "remote", "get-url", "--", remote); err == nil {
		if r, ok := ParseRemoteURL(out); ok {
			c.Repository = r
		}
	}
	return c
}

// Options are the runner's explicit attribution options.
type Options struct {
	Repository *Repository // --repository; nil when not given
	Worklog    *Worklog    // --worklog; nil when not given
}

// sameRepository reports whether two references can name the same repository.
func sameRepository(a, b Repository) bool {
	return a.Path != "" && strings.EqualFold(a.Path, b.Path) && (a.Host == "" || b.Host == "" || a.Host == b.Host)
}

// Build combines explicit options, the detected checkout and the invoking
// environment into the submitted attribution, or nil when nothing is known.
func Build(o Options, checkout Checkout, getenv func(string) string) *pb.Attribution {
	a := &pb.Attribution{Extra: map[string]string{}}
	repository, ownCheckout := checkout.Repository, true
	if o.Repository != nil {
		explicit := *o.Repository
		ownCheckout = sameRepository(explicit, repository)
		if ownCheckout && explicit.Host == "" {
			explicit.Host = repository.Host
		}
		repository = explicit
	}
	if repository.Path != "" {
		a.Repository = &repository.Path
		if repository.Host != "" {
			a.Extra[RepositoryHostKey] = repository.Host
		}
	}
	// A branch or commit of another repository's checkout would mislead.
	if ownCheckout {
		if checkout.Branch != "" {
			a.Branch = &checkout.Branch
		}
		if checkout.Commit != "" {
			a.CommitSha = &checkout.Commit
		}
	}
	if o.Worklog != nil {
		w := *o.Worklog
		if w.Repository.Path == "" {
			w.Repository = repository
		} else if w.Repository.Host == "" && sameRepository(w.Repository, repository) {
			w.Repository.Host = repository.Host
		}
		value := w.String()
		a.Worklog = &value
		if link := w.URL(); link != "" {
			a.Extra[WorklogURLKey] = link
		}
	}
	if id := getenv("COPILOT_AGENT_SESSION_ID"); sessionID.MatchString(id) {
		a.AgentSession = &id
		if agent := getenv("AI_AGENT"); agent != "" && len(agent) <= 256 {
			a.Extra[AIAgentKey] = agent
		}
	}
	if a.Repository == nil && a.Branch == nil && a.CommitSha == nil && a.Worklog == nil && a.AgentSession == nil {
		return nil
	}
	if len(a.Extra) == 0 {
		a.Extra = nil
	}
	return a
}

// WorklogLink is how to show a job's worklog. Label is e.g. owner/name#33,
// URL is the recorded issue URL if any, and Repo is the gh --repo value
// ([HOST/]OWNER/NAME) when the repository is known.
type WorklogLink struct {
	Label  string
	URL    string
	Repo   string
	Number uint64
}

// LinkWorklog reads the stored worklog attribution without consulting the
// job's working directory. ok is false when the job has no worklog.
func LinkWorklog(a *pb.Attribution) (link WorklogLink, ok bool) {
	if a.GetWorklog() == "" {
		return WorklogLink{}, false
	}
	link.Label = a.GetWorklog()
	if u, err := url.Parse(a.GetExtra()[WorklogURLKey]); err == nil && u.Scheme == "https" && u.Host != "" {
		link.URL = u.String()
	}
	w, err := ParseWorklog(a.GetWorklog())
	if err != nil {
		return link, true
	}
	link.Number = w.Number
	job := Repository{Host: a.GetExtra()[RepositoryHostKey], Path: a.GetRepository()}
	if w.Repository.Path == "" {
		w.Repository = job
	} else if w.Repository.Host == "" && sameRepository(w.Repository, job) {
		w.Repository.Host = job.Host
	}
	link.Label = w.String()
	link.Repo = w.Repository.String()
	if link.URL == "" {
		link.URL = w.URL()
	}
	return link, true
}

// SessionLink returns the Copilot app link for the job's agent session, or
// "" when the session was not reported by the Copilot app.
func SessionLink(a *pb.Attribution) string {
	if a.GetExtra()[AIAgentKey] != CopilotAppAgent || !sessionID.MatchString(a.GetAgentSession()) {
		return ""
	}
	return "ghapp://sessions/" + a.GetAgentSession()
}

// OSName is the human-readable name of a reported client OS, or "" when unreported.
func OSName(os pb.ClientOS) string {
	switch os {
	case pb.ClientOS_CLIENT_OS_MACOS:
		return "macOS"
	case pb.ClientOS_CLIENT_OS_LINUX:
		return "Linux"
	case pb.ClientOS_CLIENT_OS_WINDOWS:
		return "Windows"
	case pb.ClientOS_CLIENT_OS_OTHER:
		return "Other"
	}
	return ""
}

// ClientOS is the reported OS for a Go GOOS value.
func ClientOS(goos string) pb.ClientOS {
	switch goos {
	case "darwin":
		return pb.ClientOS_CLIENT_OS_MACOS
	case "linux":
		return pb.ClientOS_CLIENT_OS_LINUX
	case "windows":
		return pb.ClientOS_CLIENT_OS_WINDOWS
	}
	return pb.ClientOS_CLIENT_OS_OTHER
}

// ClientText formats a stored client, e.g. "tuna 2.0.0-dev · macOS 26.1 · arm64".
func ClientText(c *pb.ClientInfo) string {
	var parts []string
	if name := strings.TrimSpace(c.GetName() + " " + c.GetVersion()); name != "" {
		parts = append(parts, name)
	}
	if os := strings.TrimSpace(OSName(c.GetOs()) + " " + c.GetOsVersion()); os != "" {
		parts = append(parts, os)
	}
	if c.GetArchitecture() != pb.ClientArchitecture_CLIENT_ARCHITECTURE_UNSPECIFIED {
		parts = append(parts, strings.ToLower(strings.TrimPrefix(c.GetArchitecture().String(), "CLIENT_ARCHITECTURE_")))
	}
	return strings.Join(parts, " · ")
}
