package cli

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/tuna/apps/tuna/attribution"
	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type compatibilityServer struct {
	pb.UnimplementedControlServiceServer
	version string
}

func (s compatibilityServer) GetHealth(context.Context, *pb.GetHealthRequest) (*pb.Health, error) {
	return &pb.Health{Live: true, ServiceIdentity: contract.ServiceIdentity, ServiceVersion: s.version}, nil
}

func (compatibilityServer) GetStatus(context.Context, *pb.GetStatusRequest) (*pb.BrokerStatus, error) {
	return &pb.BrokerStatus{}, nil
}

func TestControlRequiresCompatibleBroker(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    int
	}{
		{"2.1.0", 70},
		{"3.0.0", 0},
	} {
		t.Run(tt.version, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			pb.RegisterControlServiceServer(server, compatibilityServer{version: tt.version})
			go server.Serve(listener)
			t.Cleanup(server.Stop)

			var stdout, stderr bytes.Buffer
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			code, handled := Run(context.Background(), []string{"status", "--port", port}, &stdout, &stderr, "3.0.0")
			if !handled || code != tt.want {
				t.Fatalf("status against %s = %d, handled %v; stdout %q, stderr %q",
					tt.version, code, handled, stdout.String(), stderr.String())
			}
		})
	}
}

func TestControlRejectsInvalidArgumentsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--port", "0"},
		{"pause", "unexpected"},
		{"reorder", "abc"},
		{"cancel"},
		{"status", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		code, handled := Run(context.Background(), args, &stdout, &stderr, contract.ServiceVersion)
		if !handled || code != 64 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("%v = %d,%v stdout=%q stderr=%q", args, code, handled, stdout.String(), stderr.String())
		}
	}
}

func TestHumanStatusShowsOnlyKnownLoadAverages(t *testing.T) {
	var out bytes.Buffer
	if err := printResult(&out, &pb.BrokerStatus{LoadAverage_1M: proto.Float64(0), LoadAverage_15M: proto.Float64(2.5)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Load averages: 1m=0.00 15m=2.50") || strings.Contains(out.String(), " 5m=") {
		t.Fatalf("load output = %q", out.String())
	}
}

func TestHumanJobTimesUseLocalZone(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("test", -7*60*60)
	t.Cleanup(func() { time.Local = oldLocal })
	var out bytes.Buffer
	job := &pb.Job{QueuedAt: contract.Timestamp(time.Date(2026, 11, 26, 22, 0, 0, 123456789, time.UTC).UnixNano())}
	if err := printJobDetails(&out, job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Queued: 2026-11-26T15:00:00.123456789-07:00") ||
		strings.Contains(out.String(), "Finished:") {
		t.Fatalf("local time output = %q", out.String())
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
		Attribution: &pb.Attribution{Repository: s("boumenot/tuna"), Branch: s("main"), Worklog: s("boumenot/tuna#33"),
			AgentSession: s("abc"), Extra: map[string]string{
				attribution.RepositoryHostKey: "github.com", attribution.AIAgentKey: attribution.CopilotAppAgent,
				attribution.WorklogURLKey: "https://github.com/boumenot/tuna/issues/33"}}}
	var out bytes.Buffer
	if err := printResult(&out, job); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  Repository: github.com/boumenot/tuna\n",
		"  Branch: main\n",
		"  Worklog: boumenot/tuna#33 https://github.com/boumenot/tuna/issues/33\n",
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
