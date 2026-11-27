package cli

import (
	"bytes"
	"context"
	"github.com/boumenot/heavy-lock/apps/tuna/attribution"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

func TestControlRejectsInvalidArgumentsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--port", "0"},
		{"pause", "unexpected"},
		{"reorder", "abc"},
		{"cancel"},
		{"status", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		code, handled := Run(context.Background(), args, &stdout, &stderr)
		if !handled || code != 64 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("%v = %d,%v stdout=%q stderr=%q", args, code, handled, stdout.String(), stderr.String())
		}
	}
}

func TestHumanForcedJobIsDistinct(t *testing.T) {
	var out bytes.Buffer
	if err := printJob(&out, &pb.Job{JobId: "job", Command: "echo", Forced: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "FORCED") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestHumanJobShowsRecordedAttribution(t *testing.T) {
	s := proto.String
	job := &pb.Job{JobId: "job", Command: "make", Cwd: s("/work"),
		Client: &pb.ClientInfo{Name: s("tuna"), Version: s("2.0.0"), Os: pb.ClientOS_CLIENT_OS_MACOS,
			OsVersion: s("26.1"), Architecture: pb.ClientArchitecture_CLIENT_ARCHITECTURE_ARM64},
		Attribution: &pb.Attribution{Repository: s("boumenot/heavy-lock"), Branch: s("main"), Worklog: s("boumenot/heavy-lock#33"),
			AgentSession: s("abc"), Extra: map[string]string{
				attribution.RepositoryHostKey: "github.com", attribution.AIAgentKey: attribution.CopilotAppAgent,
				attribution.WorklogURLKey: "https://github.com/boumenot/heavy-lock/issues/33"}}}
	var out bytes.Buffer
	if err := printResult(&out, job); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  Repository: github.com/boumenot/heavy-lock\n",
		"  Branch: main\n",
		"  Worklog: boumenot/heavy-lock#33 https://github.com/boumenot/heavy-lock/issues/33\n",
		"  Agent session: abc ghapp://sessions/abc\n",
		"  Client: tuna 2.0.0 · macOS 26.1 · arm64\n",
		"  Cwd: /work\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Commit") {
		t.Errorf("unrecorded commit shown:\n%s", out.String())
	}
}
