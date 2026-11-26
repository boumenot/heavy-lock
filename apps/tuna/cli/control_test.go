package cli

import (
	"bytes"
	"context"
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
