package storage

import (
	"fmt"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type legacyTimeKind uint8

const (
	legacyTimestamp legacyTimeKind = iota + 1
	legacyMilliseconds
)

// Old event payloads used varints at these same field numbers. Preserve their
// original snapshots on replay without rewriting or discarding durable events.
var legacyEventTimes = map[protoreflect.FullName]map[protowire.Number]legacyTimeKind{
	"heavylock.v2.BrokerEvent":       {2: legacyTimestamp},
	"heavylock.v2.JobEvent":          {2: legacyTimestamp},
	"heavylock.v2.ProcessIdentity":   {2: legacyTimestamp},
	"heavylock.v2.Grant":             {4: legacyTimestamp, 5: legacyMilliseconds},
	"heavylock.v2.ResourceSummary":   {1: legacyMilliseconds, 2: legacyMilliseconds},
	"heavylock.v2.Job":               {11: legacyTimestamp, 12: legacyTimestamp, 13: legacyTimestamp, 14: legacyTimestamp, 15: legacyTimestamp, 18: legacyMilliseconds, 29: legacyTimestamp},
	"heavylock.v2.BrokerTermination": {1: legacyTimestamp},
	"heavylock.v2.BrokerStatus":      {4: legacyTimestamp},
	"heavylock.v2.Heartbeat":         {1: legacyMilliseconds},
}

func restoreLegacyEvent(message protoreflect.Message) error {
	fields := legacyEventTimes[message.Descriptor().FullName()]
	unknown := message.GetUnknown()
	var remaining []byte
	for len(unknown) > 0 {
		number, wireType, size := protowire.ConsumeField(unknown)
		if size < 0 {
			return protowire.ParseError(size)
		}
		field := message.Descriptor().Fields().ByNumber(number)
		kind := fields[number]
		if kind == 0 || field == nil {
			remaining = append(remaining, unknown[:size]...)
			unknown = unknown[size:]
			continue
		}
		if wireType != protowire.VarintType {
			return fmt.Errorf("invalid wire type for legacy %s", field.FullName())
		}
		if message.Has(field) {
			return fmt.Errorf("event contains both old and new %s", field.FullName())
		}
		_, _, tagSize := protowire.ConsumeTag(unknown)
		value, valueSize := protowire.ConsumeVarint(unknown[tagSize:size])
		if valueSize < 0 {
			return protowire.ParseError(valueSize)
		}
		switch kind {
		case legacyTimestamp:
			message.Set(field, protoreflect.ValueOfMessage(contract.Timestamp(int64(value)).ProtoReflect()))
		case legacyMilliseconds:
			duration := contract.MillisecondDuration(int64(value))
			if int64(value) < 0 {
				return fmt.Errorf("negative legacy duration in %s", field.FullName())
			}
			if err := duration.CheckValid(); err != nil {
				return fmt.Errorf("invalid legacy duration in %s: %w", field.FullName(), err)
			}
			message.Set(field, protoreflect.ValueOfMessage(duration.ProtoReflect()))
		}
		unknown = unknown[size:]
	}
	message.SetUnknown(remaining)

	var conversionErr error
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind || field.IsMap() {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if conversionErr = restoreLegacyEvent(list.Get(i).Message()); conversionErr != nil {
					return false
				}
			}
		} else {
			conversionErr = restoreLegacyEvent(value.Message())
		}
		return conversionErr == nil
	})
	return conversionErr
}
