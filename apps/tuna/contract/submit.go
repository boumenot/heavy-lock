package contract

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	tunapb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type AdmissionMode uint8

const (
	AdmissionQueued AdmissionMode = 1
	AdmissionNoWait AdmissionMode = 2
	DefaultPriority int32         = 1337
)

// SubmitPriority returns the queue admission priority, including the default.
func SubmitPriority(request *tunapb.SubmitJobRequest) int32 {
	if request == nil || request.Priority == nil {
		return DefaultPriority
	}
	return request.GetPriority()
}

func CanonicalSubmitHash(request *tunapb.SubmitJobRequest, mode AdmissionMode) ([32]byte, error) {
	if request == nil {
		return [32]byte{}, errors.New("submission is required")
	}
	if mode != AdmissionQueued && mode != AdmissionNoWait {
		return [32]byte{}, fmt.Errorf("unknown admission mode %d", mode)
	}
	if hasUnknownFields(request.ProtoReflect()) {
		return [32]byte{}, errors.New("submission contains unsupported protobuf fields")
	}
	if _, err := proto.Marshal(request); err != nil {
		return [32]byte{}, fmt.Errorf("invalid submission: %w", err)
	}

	// This encoding is independent of protobuf map and wire ordering.
	encoded := append([]byte("tuna.submit.canonical\x00"), byte(mode))
	encoded = appendString(encoded, request.Command)
	encoded = appendOptionalString(encoded, request.Cwd)
	encoded = binary.BigEndian.AppendUint32(encoded, request.Weight)

	client := request.Client
	if client == nil {
		client = &tunapb.ClientInfo{}
	}
	encoded = appendOptionalString(encoded, client.Name)
	encoded = appendOptionalString(encoded, client.Version)
	encoded = binary.BigEndian.AppendUint32(encoded, uint32(client.Os))
	encoded = binary.BigEndian.AppendUint32(encoded, uint32(client.Architecture))

	attribution := request.Attribution
	if attribution == nil {
		attribution = &tunapb.Attribution{}
	}
	for _, field := range []*string{
		attribution.Repository,
		attribution.Branch,
		attribution.Worklog,
		attribution.AgentSession,
		attribution.Purpose,
		attribution.CommitSha,
		attribution.BuildTarget,
		attribution.BuildConfiguration,
	} {
		encoded = appendOptionalString(encoded, field)
	}
	keys := make([]string, 0, len(attribution.Extra))
	for key := range attribution.Extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(keys)))
	for _, key := range keys {
		encoded = appendString(encoded, key)
		encoded = appendString(encoded, attribution.Extra[key])
	}

	if request.Runner == nil {
		encoded = append(encoded, 0)
	} else {
		encoded = append(encoded, 1)
		encoded = appendString(encoded, request.Runner.IncarnationId)
		if request.Runner.Process == nil {
			encoded = append(encoded, 0)
		} else {
			start, err := UnixNano(request.Runner.Process.StartTime)
			if err != nil {
				return [32]byte{}, fmt.Errorf("invalid runner process start time: %w", err)
			}
			encoded = append(encoded, 1)
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(request.Runner.Process.Pid))
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(start))
		}
	}
	encoded = appendOptionalString(encoded, request.RetryOfJobId)
	// Fields added after the original encoding follow as named extensions only when
	// present, so hashes of earlier submissions remain unchanged.
	if client.OsVersion != nil {
		encoded = appendString(encoded, "client.os_version")
		encoded = appendString(encoded, *client.OsVersion)
	}
	if priority := SubmitPriority(request); priority != DefaultPriority {
		encoded = appendString(encoded, "priority")
		encoded = binary.BigEndian.AppendUint32(encoded, uint32(priority))
	}
	return sha256.Sum256(encoded), nil
}

func RenderCommand(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("command requires at least one argument")
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') {
			return "", errors.New("command arguments must be valid UTF-8 without NUL")
		}
		safe := arg != ""
		for i := 0; i < len(arg); i++ {
			c := arg[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' || strings.ContainsRune("_@%+=:,./-", rune(c))) {
				safe = false
				break
			}
		}
		if safe {
			quoted = append(quoted, arg)
		} else {
			quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", "'\\''")+"'")
		}
	}
	return strings.Join(quoted, " "), nil
}

func appendString(dst []byte, value string) []byte {
	dst = binary.BigEndian.AppendUint64(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendOptionalString(dst []byte, value *string) []byte {
	if value == nil {
		return append(dst, 0)
	}
	return appendString(append(dst, 1), *value)
}

func hasUnknownFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	var lastSupported protoreflect.FieldNumber
	switch message.Descriptor().FullName() {
	case "tuna.SubmitJobRequest":
		lastSupported = 9
	case "tuna.ClientInfo":
		lastSupported = 5
	case "tuna.Attribution":
		lastSupported = 9
	case "tuna.RunnerIncarnation", "tuna.ProcessIdentity", "google.protobuf.Timestamp":
		lastSupported = 2
	default:
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Number() > lastSupported {
			unknown = true
			return false
		}
		if field.IsMap() && field.MapValue().Kind() == protoreflect.MessageKind {
			value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
				unknown = hasUnknownFields(item.Message())
				return !unknown
			})
		} else if field.IsList() && field.Kind() == protoreflect.MessageKind {
			list := value.List()
			for i := 0; i < list.Len() && !unknown; i++ {
				unknown = hasUnknownFields(list.Get(i).Message())
			}
		} else if !field.IsMap() && !field.IsList() && field.Kind() == protoreflect.MessageKind {
			unknown = hasUnknownFields(value.Message())
		}
		return !unknown
	})
	return unknown
}
