// Package cli provides noninteractive control commands for the broker.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/attribution"
	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/transport"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const Usage = "Controls: tuna <status|queue|jobs|job|pause|resume|reorder|cancel|force-start> [options]\n" +
	"  --port PORT (default 49783), --json, --timeout DURATION (default 5s)\n" +
	"  job/cancel/force-start: --job UUID; cancel: --reason TEXT\n" +
	"  reorder: --job UUID and one of --front, --back, --before UUID, --after UUID\n" +
	"  jobs: --page-size N, --page-token TOKEN (newest first)\n" +
	"Controls never prompt. Cancel and force-start execute immediately.\n"

// Run recognizes only controls, leaving runner/service dispatch to main.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, clientVersion string) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	command := args[0]
	switch command {
	case "status", "queue", "jobs", "job", "pause", "resume", "reorder", "cancel", "force-start":
	default:
		return 0, false
	}
	fail := func(code int, err error) (int, bool) {
		if _, writeErr := fmt.Fprintln(stderr, "tuna:", err); writeErr != nil {
			return 70, true
		}
		return code, true
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", transport.DefaultPort, "broker port")
	jsonOutput := fs.Bool("json", false, "protobuf JSON")
	timeout := fs.Duration("timeout", 5*time.Second, "total RPC deadline")
	var job, before, after, reason, token string
	var front, back bool
	var pageSize uint
	switch command {
	case "job", "cancel", "force-start", "reorder":
		fs.StringVar(&job, "job", "", "job UUID")
	}
	if command == "cancel" {
		fs.StringVar(&reason, "reason", "", "cancellation reason")
	}
	if command == "reorder" {
		fs.BoolVar(&front, "front", false, "move to front")
		fs.BoolVar(&back, "back", false, "move to back")
		fs.StringVar(&before, "before", "", "move before job")
		fs.StringVar(&after, "after", "", "move after job")
	}
	if command == "jobs" {
		fs.UintVar(&pageSize, "page-size", 50, "page size")
		fs.StringVar(&token, "page-token", "", "page cursor")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			if _, err := io.WriteString(stdout, Usage); err != nil {
				return 70, true
			}
			return 0, true
		}
		return fail(64, err)
	}
	if fs.NArg() != 0 || *port < 1 || *port > 65535 || *timeout <= 0 || pageSize > 500 {
		return fail(64, fmt.Errorf("invalid arguments; use tuna %s --help", command))
	}
	if (command == "job" || command == "cancel" || command == "force-start" || command == "reorder") && job == "" {
		return fail(64, fmt.Errorf("--job is required"))
	}
	if command == "reorder" {
		targets := 0
		for _, present := range []bool{front, back, before != "", after != ""} {
			if present {
				targets++
			}
		}
		if targets != 1 {
			return fail(64, fmt.Errorf("reorder needs exactly one target"))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	conn, err := grpc.NewClient("127.0.0.1:"+strconv.Itoa(*port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fail(70, err)
	}
	defer conn.Close()
	client := pb.NewControlServiceClient(conn)
	health, err := client.GetHealth(ctx, &pb.GetHealthRequest{})
	if err == nil {
		// Status and queue output use the explicit state introduced in API 2.1.
		err = contract.CheckService(health, clientVersion, 1)
	}
	if err != nil {
		return fail(70, fmt.Errorf("broker unavailable or incompatible: %w", err))
	}
	var result proto.Message
	switch command {
	case "status":
		result, err = client.GetStatus(ctx, &pb.GetStatusRequest{})
	case "queue":
		result, err = client.GetQueue(ctx, &pb.GetQueueRequest{})
	case "jobs":
		result, err = client.ListJobs(ctx, &pb.ListJobsRequest{PageSize: uint32(pageSize), PageToken: token})
	case "job":
		result, err = client.GetJob(ctx, &pb.GetJobRequest{JobId: job})
	case "pause":
		result, err = client.PauseQueue(ctx, &pb.PauseQueueRequest{})
	case "resume":
		result, err = client.ResumeQueue(ctx, &pb.ResumeQueueRequest{})
	case "cancel":
		req := &pb.CancelJobRequest{JobId: job}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "reason" {
				req.Reason = &reason
			}
		})
		result, err = client.CancelJob(ctx, req)
	case "force-start":
		result, err = client.ForceStartJob(ctx, &pb.ForceStartJobRequest{JobId: job})
	case "reorder":
		req := &pb.MoveJobRequest{JobId: job}
		switch {
		case front:
			req.Target = &pb.MoveJobRequest_ToFront{ToFront: true}
		case back:
			req.Target = &pb.MoveJobRequest_ToBack{ToBack: true}
		case before != "":
			req.Target = &pb.MoveJobRequest_BeforeJobId{BeforeJobId: before}
		case after != "":
			req.Target = &pb.MoveJobRequest_AfterJobId{AfterJobId: after}
		}
		result, err = client.MoveJob(ctx, req)
	}
	if err != nil {
		s := status.Convert(contract.NormalizeError(err))
		for _, detail := range s.Details() {
			if info, ok := detail.(*errdetails.ErrorInfo); ok {
				return fail(70, fmt.Errorf("%s/%s: %s", s.Code(), info.Reason, s.Message()))
			}
		}
		return fail(70, err)
	}
	if result == nil || !result.ProtoReflect().IsValid() {
		return fail(70, fmt.Errorf("broker returned an empty response"))
	}
	if *jsonOutput {
		data, err := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(result)
		if err != nil {
			return fail(70, err)
		}
		if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
			return fail(70, err)
		}
	} else if err := printResult(stdout, result); err != nil {
		return fail(70, err)
	}
	return 0, true
}

func printJob(w io.Writer, job *pb.Job) error {
	forced := ""
	if job.Forced {
		forced = " FORCED"
	}
	_, err := fmt.Fprintf(w, "%s %s weight=%d%s command=%q blocked=%s\n", job.JobId,
		strings.TrimPrefix(job.State.String(), "JOB_STATE_"), job.Weight, forced, job.Command,
		strings.TrimPrefix(job.BlockedReason.String(), "BLOCKED_REASON_"))
	return err
}

// printJobDetails adds the job's recorded attribution and client, one
// "  label: value" line each, omitting values that were not recorded.
func printJobDetails(w io.Writer, job *pb.Job) error {
	a := job.Attribution
	var lines [][2]string
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, [2]string{label, value})
		}
	}
	if a.GetRepository() != "" {
		add("Repository", attribution.Repository{Host: a.GetExtra()[attribution.RepositoryHostKey], Path: a.GetRepository()}.String())
	}
	add("Branch", a.GetBranch())
	add("Commit", a.GetCommitSha())
	if link, ok := attribution.LinkWorklog(a); ok {
		add("Worklog", strings.TrimSpace(link.Label+" "+link.URL))
	}
	if a.GetAgentSession() != "" {
		add("Agent session", strings.TrimSpace(a.GetAgentSession()+" "+attribution.SessionLink(a)))
	}
	add("Client", attribution.ClientText(job.Client))
	add("Cwd", job.GetCwd())
	for _, field := range []struct {
		label string
		time  *timestamppb.Timestamp
	}{
		{"Queued", job.QueuedAt},
		{"Admitted", job.AdmittedAt},
		{"Started", job.StartedAt},
		{"Finished", job.FinishedAt},
	} {
		if field.time == nil {
			continue
		}
		if err := field.time.CheckValid(); err != nil {
			return fmt.Errorf("invalid %s timestamp: %w", field.label, err)
		}
		add(field.label, field.time.AsTime().Local().Format(time.RFC3339Nano))
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(w, "  %s: %s\n", line[0], line[1]); err != nil {
			return err
		}
	}
	return nil
}

func printResult(w io.Writer, result proto.Message) error {
	switch value := result.(type) {
	case *pb.Job:
		if err := printJob(w, value); err != nil {
			return err
		}
		return printJobDetails(w, value)
	case *pb.Queue:
		if _, err := fmt.Fprintf(w, "Queue: %d jobs; state: %s; head blocked=%s\n", len(value.Jobs),
			enumName(value.QueueState.String(), "QUEUE_STATE_"), value.HeadBlockedReason); err != nil {
			return err
		}
		for _, job := range value.Jobs {
			if err := printJob(w, job); err != nil {
				return err
			}
		}
	case *pb.ListJobsResponse:
		for _, job := range value.Jobs {
			if err := printJob(w, job); err != nil {
				return err
			}
		}
		if value.NextPageToken != "" {
			_, err := fmt.Fprintln(w, "Next page token:", value.NextPageToken)
			return err
		}
	case *pb.BrokerStatus:
		if _, err := fmt.Fprintf(w, "Mode: %s; state: %s; capacity: %d; occupied: %d; queued: %d; running: %d; recovering: %t; storage unavailable: %t\n",
			enumName(value.QueueMode.String(), "QUEUE_MODE_"), enumName(value.QueueState.String(), "QUEUE_STATE_"),
			value.Capacity, value.Occupied, value.QueuedJobs, value.RunningJobs, value.Recovering, value.StorageUnavailable); err != nil {
			return err
		}
		var loads []string
		for _, field := range []struct {
			label string
			value *float64
		}{
			{"1m", value.LoadAverage_1M},
			{"5m", value.LoadAverage_5M},
			{"15m", value.LoadAverage_15M},
		} {
			if field.value != nil {
				loads = append(loads, fmt.Sprintf("%s=%.2f", field.label, *field.value))
			}
		}
		if len(loads) == 0 {
			return nil
		}
		_, err := fmt.Fprintln(w, "Load averages:", strings.Join(loads, " "))
		return err
	default:
		return fmt.Errorf("unsupported response %T", result)
	}
	return nil
}

// enumName prints an enum value in lower case without its prefix; an older
// broker that omits the field reports "unknown".
func enumName(name, prefix string) string {
	name = strings.ToLower(strings.TrimPrefix(name, prefix))
	if name == "unspecified" {
		return "unknown"
	}
	return name
}
