package service

import (
	"encoding/base64"
	"strings"
)

// Protobuf wire types supported by the hand-rolled dagql call parser.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// Field numbers from dagger/dagger dagql/call/callpbv1. Only the fields the
// exec view-model reads are named; the rest are skipped.
const (
	callFieldNum     = 3 // Call.field
	callArgsNum      = 4 // Call.args (repeated Argument)
	argNameNum       = 1 // Argument.name
	argValueNum      = 2 // Argument.value (Literal)
	literalStringNum = 7 // Literal.string
	literalListNum   = 8 // Literal.list
	listValuesNum    = 1 // List.values (repeated Literal)
)

// protoField is one decoded top-level protobuf wire field. Only the fields the
// dagql call parser reads are retained; varint values are consumed and
// discarded.
type protoField struct {
	num   int
	wire  int
	bytes []byte
}

// parseProtoFields decodes the top-level wire fields of a protobuf message. It
// is panic-free: malformed input (field number 0, truncated varint/length,
// unknown wire type) yields ok=false.
func parseProtoFields(data []byte) ([]protoField, bool) {
	var fields []protoField
	i := 0
	for i < len(data) {
		key, next, ok := readVarint(data, i)
		if !ok {
			return nil, false
		}
		i = next
		num := int(key >> 3)
		wire := int(key & 0x7)
		if num == 0 {
			return nil, false
		}
		switch wire {
		case wireVarint:
			_, next, ok := readVarint(data, i)
			if !ok {
				return nil, false
			}
			i = next
			fields = append(fields, protoField{num: num, wire: wire})
		case wireFixed64:
			if i+8 > len(data) {
				return nil, false
			}
			fields = append(fields, protoField{num: num, wire: wire, bytes: data[i : i+8]})
			i += 8
		case wireBytes:
			length, next, ok := readVarint(data, i)
			if !ok {
				return nil, false
			}
			i = next
			remaining := len(data) - i
			if length > uint64(remaining) { //nolint:gosec // remaining is non-negative.
				return nil, false
			}
			end := i + int(length) //nolint:gosec // length <= remaining, so it fits in int.
			fields = append(fields, protoField{num: num, wire: wire, bytes: data[i:end]})
			i = end
		case wireFixed32:
			if i+4 > len(data) {
				return nil, false
			}
			fields = append(fields, protoField{num: num, wire: wire, bytes: data[i : i+4]})
			i += 4
		default:
			return nil, false
		}
	}
	return fields, true
}

// readVarint reads a base-128 varint starting at off, returning the value and
// the offset just past it. ok is false on truncation or an over-long varint.
func readVarint(data []byte, off int) (value uint64, next int, ok bool) {
	var shift uint
	for i := off; i < len(data); i++ {
		if shift >= 64 {
			return 0, 0, false
		}
		b := data[i]
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, i + 1, true
		}
		shift += 7
	}
	return 0, 0, false
}

// protoBytes returns the first length-delimited field with the given number.
func protoBytes(fields []protoField, num int) []byte {
	for _, f := range fields {
		if f.num == num && f.wire == wireBytes {
			return f.bytes
		}
	}
	return nil
}

// protoString returns the first length-delimited field with the given number
// as a string.
func protoString(fields []protoField, num int) string {
	return string(protoBytes(fields, num))
}

// parseLiteralStrings decodes a callpbv1.Literal into its string values: a
// scalar string at field 7, or a list at field 8 whose repeated field 1 items
// each carry a string at field 7. Non-string literals yield no values.
func parseLiteralStrings(data []byte) []string {
	fields, ok := parseProtoFields(data)
	if !ok {
		return nil
	}
	var out []string
	for _, f := range fields {
		if f.wire != wireBytes {
			continue
		}
		switch f.num {
		case literalStringNum:
			out = append(out, string(f.bytes))
		case literalListNum:
			listFields, ok := parseProtoFields(f.bytes)
			if !ok {
				continue
			}
			for _, lf := range listFields {
				if lf.num == listValuesNum && lf.wire == wireBytes {
					out = append(out, parseLiteralStrings(lf.bytes)...)
				}
			}
		}
	}
	return out
}

// parseDagCall decodes a base64-encoded callpbv1.Call (the value of the
// dagger.io/dag.call span attribute) into its field name and named argument
// values. ok is false on decode/parse failure or when the call carries neither
// a field nor any argument.
func parseDagCall(raw string) (field string, args map[string][]string, ok bool) {
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", nil, false
	}
	fields, ok := parseProtoFields(data)
	if !ok {
		return "", nil, false
	}
	field = protoString(fields, callFieldNum)
	args = make(map[string][]string)
	for _, f := range fields {
		if f.num != callArgsNum || f.wire != wireBytes {
			continue
		}
		argFields, ok := parseProtoFields(f.bytes)
		if !ok {
			continue
		}
		name := protoString(argFields, argNameNum)
		if name == "" {
			continue
		}
		if values := parseLiteralStrings(protoBytes(argFields, argValueNum)); len(values) > 0 {
			args[name] = values
		}
	}
	if field == "" && len(args) == 0 {
		return "", nil, false
	}
	return field, args, true
}

// isExecCallField reports whether a dagql call field names an exec operation.
func isExecCallField(field string) bool {
	switch strings.ToLower(field) {
	case "withexec", "exec":
		return true
	default:
		return false
	}
}

// isIOOperationField reports whether a dagql call field names an io operation
// (publish/export/import/push) whose target rides an address argument.
func isIOOperationField(field string) bool {
	switch strings.ToLower(field) {
	case "publish", "export", "import", "push":
		return true
	default:
		return false
	}
}
