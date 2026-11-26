package llm

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
)

const (
	DevinDefaultBaseURL  = "https://server.codeium.com"
	DevinDefaultModel    = "swe-1-6"
	devinChatMessagePath = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	devinAuthPath        = "/exa.auth_pb.AuthService/GetUserJwt"
	devinAssignModelPath = "/exa.api_server_pb.ApiServerService/AssignModel"
	devinModelConfigsPath = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"

	devinSessionTokenPrefix = "devin-session-token$"
	devinToolCallIDMaxLen   = 40

	// ChatMessageSource enum values.
	devinChatSourceUser   uint64 = 1
	devinChatSourceSystem uint64 = 2
	devinChatSourceTool   uint64 = 4

	// ChatMessageRequestType::CASCADE.
	devinRequestTypeCascade uint64 = 5
	// ConversationalPlannerMode::DEFAULT.
	devinPlannerModeDefault uint64 = 1
	// CacheControlType::EPHEMERAL.
	devinCacheControlEphemeral uint64 = 1
	// StopReason::MAX_TOKENS.
	devinStopReasonMaxTokens uint64 = 3
	// DisplayOption::MODEL_ROUTER.
	devinDisplayOptionModelRouter uint64 = 3

	// Compactable history byte threshold to treat opaque internal invalid_argument as context overflow.
	devinLargeHistoryRecoveryBytes = 512 * 1024
)

var devinSupportedModelDisplays = []uint64{3, 4, 6, 7, 8}

func isDevinInternalDisplay(opt uint64) bool {
	return opt == 4 || opt == 6
}

func encodeDevinCredential(apiKey string) string {
	key := strings.TrimSpace(apiKey)
	if key == "" || strings.HasPrefix(key, devinSessionTokenPrefix) {
		return key
	}
	if strings.Count(key, ".") == 2 {
		return devinSessionTokenPrefix + key
	}
	return key
}

func devinOSName() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// ---------- minimal pure-Go protobuf wire codec ----------

const (
	protoWireVarint uint8 = 0
	protoWire64Bit  uint8 = 1
	protoWireLength uint8 = 2
	protoWire32Bit  uint8 = 5
)

func protoAppendVarint(out []byte, v uint64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

func protoAppendKey(out []byte, field uint32, wire uint8) []byte {
	return protoAppendVarint(out, (uint64(field)<<3)|uint64(wire))
}

func protoAppendVarintField(out []byte, field uint32, v uint64) []byte {
	if v == 0 {
		return out
	}
	out = protoAppendKey(out, field, protoWireVarint)
	return protoAppendVarint(out, v)
}

func protoAppendBoolField(out []byte, field uint32, v bool) []byte {
	if !v {
		return out
	}
	out = protoAppendKey(out, field, protoWireVarint)
	return append(out, 1)
}

func protoAppendDoubleField(out []byte, field uint32, v float64) []byte {
	out = protoAppendKey(out, field, protoWire64Bit)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	return append(out, b[:]...)
}

func protoAppendLenField(out []byte, field uint32, data []byte) []byte {
	if len(data) == 0 {
		return out
	}
	out = protoAppendKey(out, field, protoWireLength)
	out = protoAppendVarint(out, uint64(len(data)))
	return append(out, data...)
}

func protoAppendStringField(out []byte, field uint32, s string) []byte {
	if s == "" {
		return out
	}
	out = protoAppendKey(out, field, protoWireLength)
	out = protoAppendVarint(out, uint64(len(s)))
	return append(out, s...)
}

func protoAppendPackedVarints(out []byte, field uint32, values []uint64) []byte {
	if len(values) == 0 {
		return out
	}
	packed := make([]byte, 0, len(values)*2)
	for _, v := range values {
		packed = protoAppendVarint(packed, v)
	}
	return protoAppendLenField(out, field, packed)
}

type protoField struct {
	number uint32
	wire   uint8
	varint uint64
	bytes  []byte
}

func (f protoField) str() string {
	return string(f.bytes)
}

func decodeProtoFields(buf []byte) []protoField {
	var out []protoField
	pos := 0
	readVarint := func() (uint64, bool) {
		var val uint64
		var shift uint
		for pos < len(buf) {
			b := buf[pos]
			pos++
			val |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				return val, true
			}
			shift += 7
			if shift >= 64 {
				return 0, false
			}
		}
		return 0, false
	}

	for pos < len(buf) {
		key, ok := readVarint()
		if !ok {
			break
		}
		num := uint32(key >> 3)
		wire := uint8(key & 0x07)
		switch wire {
		case protoWireVarint:
			v, ok := readVarint()
			if !ok {
				return out
			}
			out = append(out, protoField{number: num, wire: wire, varint: v})
		case protoWire64Bit:
			if pos+8 > len(buf) {
				return out
			}
			v := binary.LittleEndian.Uint64(buf[pos : pos+8])
			pos += 8
			out = append(out, protoField{number: num, wire: wire, varint: v})
		case protoWireLength:
			l, ok := readVarint()
			if !ok {
				return out
			}
			if l > uint64(len(buf)-pos) {
				return out
			}
			end := pos + int(l)
			out = append(out, protoField{number: num, wire: wire, bytes: buf[pos:end]})
			pos = end
		case protoWire32Bit:
			if pos+4 > len(buf) {
				return out
			}
			v := uint64(binary.LittleEndian.Uint32(buf[pos : pos+4]))
			pos += 4
			out = append(out, protoField{number: num, wire: wire, varint: v})
		default:
			return out
		}
	}
	return out
}

// ---------- Connect protocol framing ----------

const (
	connectFlagCompressed byte = 0x01
	connectFlagEndStream  byte = 0x02
	maxConnectFrameBytes       = 32 * 1024 * 1024
)

func frameConnectMessage(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0x00 // uncompressed data frame
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func readConnectFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	flags := hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > maxConnectFrameBytes {
		return 0, nil, fmt.Errorf("devin: connect frame too large (%d bytes)", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	if flags&connectFlagCompressed != 0 && len(payload) > 0 {
		gz, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return 0, nil, fmt.Errorf("devin: gzip frame header: %w", err)
		}
		decompressed, err := io.ReadAll(io.LimitReader(gz, maxConnectFrameBytes+1))
		_ = gz.Close()
		if err != nil {
			return 0, nil, fmt.Errorf("devin: gzip frame read: %w", err)
		}
		if len(decompressed) > maxConnectFrameBytes {
			return 0, nil, fmt.Errorf("devin: decompressed frame exceeds limit")
		}
		payload = decompressed
	}
	return flags, payload, nil
}

// ---------- Cascade message encoders ----------

func encodeDevinCLIMetadata(apiKey, userJWT string) []byte {
	out := make([]byte, 0, 160)
	out = protoAppendStringField(out, 1, "devin-cli")
	out = protoAppendStringField(out, 2, "3000.11.205")
	out = protoAppendStringField(out, 3, encodeDevinCredential(apiKey))
	out = protoAppendStringField(out, 4, "en")
	out = protoAppendStringField(out, 5, devinOSName())
	out = protoAppendStringField(out, 7, "3000.11.205")
	out = protoAppendStringField(out, 12, "chisel")
	out = protoAppendStringField(out, 21, userJWT)
	out = protoAppendStringField(out, 28, "chisel")
	return out
}

func encodeDevinDiscoveryMetadata(apiKey string) []byte {
	out := make([]byte, 0, 160)
	out = protoAppendStringField(out, 1, "chisel")
	out = protoAppendStringField(out, 2, "0.0.0-dev")
	out = protoAppendStringField(out, 3, encodeDevinCredential(apiKey))
	out = protoAppendStringField(out, 4, "en")
	out = protoAppendStringField(out, 5, devinOSName())
	out = protoAppendStringField(out, 7, "0.0.0-dev")
	out = protoAppendStringField(out, 12, "chisel")
	out = protoAppendPackedVarints(out, 30, devinSupportedModelDisplays)
	return out
}

func encodeDevinUnaryRequest(metadata []byte) []byte {
	out := make([]byte, 0, len(metadata)+8)
	return protoAppendLenField(out, 1, metadata)
}

func encodeDevinToolCall(id, name, argsJSON string) []byte {
	out := make([]byte, 0, len(id)+len(name)+len(argsJSON)+16)
	out = protoAppendStringField(out, 1, id)
	out = protoAppendStringField(out, 2, name)
	out = protoAppendStringField(out, 3, argsJSON)
	return out
}

func encodeDevinPromptCacheOptions() []byte {
	return protoAppendVarintField(nil, 1, devinCacheControlEphemeral)
}

type devinChatPromptParts struct {
	messageID         string
	source            uint64
	prompt            string
	thinking          string
	signature         string
	signatureType     string
	toolCalls         [][]byte
	toolCallID        string
	toolResultIsError bool
}

func encodeDevinChatMessagePrompt(p devinChatPromptParts) []byte {
	out := make([]byte, 0, len(p.prompt)+len(p.thinking)+64)
	out = protoAppendStringField(out, 1, p.messageID)
	out = protoAppendVarintField(out, 2, p.source)
	out = protoAppendStringField(out, 3, p.prompt)
	for _, call := range p.toolCalls {
		out = protoAppendLenField(out, 6, call)
	}
	out = protoAppendStringField(out, 7, p.toolCallID)
	out = protoAppendBoolField(out, 9, p.toolResultIsError)
	out = protoAppendStringField(out, 11, p.thinking)
	out = protoAppendStringField(out, 12, p.signature)
	out = protoAppendStringField(out, 18, p.signatureType)
	return out
}

func encodeDevinToolDefinition(name, desc string, schema map[string]any, strict bool) []byte {
	raw, _ := json.Marshal(schema)
	out := make([]byte, 0, len(name)+len(desc)+len(raw)+16)
	out = protoAppendStringField(out, 1, name)
	out = protoAppendStringField(out, 2, desc)
	out = protoAppendStringField(out, 3, string(raw))
	out = protoAppendBoolField(out, 12, strict)
	return out
}

func encodeDevinCompletionConfiguration(maxTokens uint64) []byte {
	out := make([]byte, 0, 48)
	out = protoAppendVarintField(out, 1, 1) // num_completions
	out = protoAppendVarintField(out, 2, maxTokens)
	out = protoAppendVarintField(out, 3, 400) // max_newlines
	out = protoAppendDoubleField(out, 5, 1.0) // temperature
	out = protoAppendVarintField(out, 7, 40)  // top_k
	out = protoAppendDoubleField(out, 8, float64(float32(0.95)))
	return out
}

func encodeDevinToolChoiceAuto() []byte {
	return protoAppendStringField(nil, 1, "auto")
}

// ---------- UUID & sanitization helpers ----------

func devinFormatUUID(b [16]byte) string {
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3],
		b[4], b[5],
		b[6], b[7],
		b[8], b[9],
		b[10], b[11], b[12], b[13], b[14], b[15],
	)
}

func devinDeterministicUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var b [16]byte
	copy(b[:], sum[:16])
	return devinFormatUUID(b)
}

func devinRandomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return devinFormatUUID(b)
}

// devinWireToolCallID sanitizes tool call IDs to [A-Za-z0-9_-] and bounds them
// to 40 chars with a deterministic 16-hex SHA-256 suffix when longer.
func devinWireToolCallID(id string) string {
	var sb strings.Builder
	sb.Grow(len(id))
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	sanitized := sb.String()
	if len(sanitized) <= devinToolCallIDMaxLen {
		return sanitized
	}
	const hashLen = 16
	headLen := devinToolCallIDMaxLen - 1 - hashLen
	sum := sha256.Sum256([]byte(id))
	return sanitized[:headLen] + "_" + hex.EncodeToString(sum[:8])
}

func isDevinGeminiUID(uid string) bool {
	return strings.HasPrefix(uid, "MODEL_GOOGLE_GEMINI_") || strings.Contains(strings.ToLower(uid), "gemini")
}

func isDevinClaudeUID(uid string) bool {
	return strings.Contains(strings.ToLower(uid), "claude")
}

// scalarizeSchemaTypes converts JSON Schema type arrays like ["number", "null"]
// into {"type": "number", "nullable": true} for Gemini behind Cascade.
func scalarizeSchemaTypes(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node)+1)
		for k, val := range node {
			out[k] = scalarizeSchemaTypes(val)
		}
		if arr, ok := out["type"].([]any); ok {
			nullable := false
			var first string
			for _, elem := range arr {
				s, _ := elem.(string)
				if s == "null" {
					nullable = true
				} else if first == "" && s != "" {
					first = s
				}
			}
			if first != "" {
				out["type"] = first
				if nullable {
					out["nullable"] = true
				}
			}
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = scalarizeSchemaTypes(item)
		}
		return out
	default:
		return v
	}
}

func stripTopLevelCombinators(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		if k == "oneOf" || k == "anyOf" || k == "allOf" {
			continue
		}
		out[k] = v
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out
}

// ---------- response decoders ----------

type devinUserJWT struct {
	userJWT            string
	customAPIServerURL string
}

func decodeDevinGetUserJWT(b []byte) (devinUserJWT, bool) {
	var res devinUserJWT
	for _, f := range decodeProtoFields(b) {
		switch f.number {
		case 1:
			res.userJWT = f.str()
		case 2:
			res.customAPIServerURL = f.str()
		}
	}
	return res, res.userJWT != ""
}

type devinModelAssignment struct {
	assignmentJWT string
	modelUID      string
}

func decodeDevinAssignModel(b []byte) (devinModelAssignment, bool) {
	for _, f := range decodeProtoFields(b) {
		if f.number != 1 {
			continue
		}
		var a devinModelAssignment
		for _, nested := range decodeProtoFields(f.bytes) {
			switch nested.number {
			case 1:
				a.assignmentJWT = nested.str()
			case 2:
				a.modelUID = nested.str()
			}
		}
		if a.assignmentJWT != "" && a.modelUID != "" {
			return a, true
		}
	}
	return devinModelAssignment{}, false
}

func decodeDevinUnary[T any](payload []byte, decode func([]byte) (T, bool)) (T, bool) {
	if v, ok := decode(payload); ok {
		return v, true
	}
	if len(payload) >= 5 {
		length := int(binary.BigEndian.Uint32(payload[1:5]))
		if length == len(payload)-5 {
			return decode(payload[5:])
		}
	}
	var zero T
	return zero, false
}

type devinToolCallDelta struct {
	id   string
	name string
	args string
}

type devinChatDelta struct {
	deltaText        string
	deltaThinking    string
	deltaSignature   string
	stopReason       uint64
	toolCalls        []devinToolCallDelta
	inputTokens      uint64
	outputTokens     uint64
	cacheWriteTokens uint64
	cacheReadTokens  uint64
	hasUsage         bool
}

func decodeDevinChatDelta(b []byte) devinChatDelta {
	var d devinChatDelta
	for _, f := range decodeProtoFields(b) {
		switch f.number {
		case 3:
			d.deltaText = f.str()
		case 5:
			d.stopReason = f.varint
		case 6:
			var tc devinToolCallDelta
			for _, cf := range decodeProtoFields(f.bytes) {
				switch cf.number {
				case 1:
					tc.id = cf.str()
				case 2:
					tc.name = cf.str()
				case 3:
					tc.args = cf.str()
				}
			}
			d.toolCalls = append(d.toolCalls, tc)
		case 7:
			for _, uf := range decodeProtoFields(f.bytes) {
				switch uf.number {
				case 2:
					d.inputTokens = uf.varint
				case 3:
					d.outputTokens = uf.varint
				case 4:
					d.cacheWriteTokens = uf.varint
				case 5:
					d.cacheReadTokens = uf.varint
				}
			}
			d.hasUsage = true
		case 9:
			d.deltaThinking = f.str()
		case 10:
			d.deltaSignature = f.str()
		}
	}
	return d
}
