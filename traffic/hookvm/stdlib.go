package hookvm

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const regexpCompileCost = 32 << 10

func (e *vm) checkRepeat(elemLen, cnt int64) int {
	if cnt < 0 {
		e.failf("Repeat: negative count")
	}
	if cnt > 0 && elemLen > e.memLimit/cnt {
		e.fail(ErrMemoryLimit)
	}
	e.alloc(elemLen * cnt)
	return int(cnt)
}

func (e *vm) checkReplaceAll(srcLen, oldLen, newLen, count int64) {
	if count <= 0 {
		e.alloc(srcLen)
		return
	}
	delta := newLen - oldLen
	if delta > 0 {
		if delta > e.memLimit/count {
			e.fail(ErrMemoryLimit)
		}
		add := count * delta
		if srcLen > e.memLimit-add {
			e.fail(ErrMemoryLimit)
		}
		e.alloc(srcLen + add)
		return
	}
	projected := srcLen + count*delta
	if projected < 0 {
		projected = 0
	}
	e.alloc(projected)
}

func (e *vm) checkJSONBudget(v any) {
	var est int64
	rem := e.memLimit - e.memUsed
	if rem < 0 {
		e.fail(ErrMemoryLimit)
	}
	charge := func(n int64) {
		if n < 0 {
			return
		}
		if n > rem || est > rem-n {
			e.fail(ErrMemoryLimit)
		}
		est += n
	}
	var walk func(x any, depth int)
	walk = func(x any, depth int) {
		if depth > 256 {
			e.fail(ErrMemoryLimit)
		}
		switch val := x.(type) {
		case nil, bool:
			charge(5)
		case int, int64, float64:
			charge(24)
		case string:
			charge(int64(len(val)) + 2)
		case []byte:
			charge(int64(base64.StdEncoding.EncodedLen(len(val))) + 2)
		case []any:
			charge(2 + int64(len(val)))
			for _, elem := range val {
				walk(elem, depth+1)
			}
		case []string:
			charge(2 + int64(len(val)))
			for _, s := range val {
				charge(int64(len(s)) + 2)
			}
		case map[string]any:
			charge(2 + int64(len(val))*2)
			for k, elem := range val {
				charge(int64(len(k)) + 3)
				walk(elem, depth+1)
			}
		case map[string][]string:
			charge(2 + int64(len(val))*2)
			for k, ss := range val {
				charge(int64(len(k)) + 5 + int64(len(ss)))
				for _, s := range ss {
					charge(int64(len(s)) + 2)
				}
			}
		default:
			charge(32)
		}
	}
	walk(v, 0)
}

func (e *vm) checkQueryBudget(m map[string][]string) {
	rem := e.memLimit - e.memUsed
	if rem < 0 {
		e.fail(ErrMemoryLimit)
	}
	var est int64
	charge := func(n int64) {
		if n < 0 {
			return
		}
		if n > rem || est > rem-n {
			e.fail(ErrMemoryLimit)
		}
		est += n
	}
	charge3x := func(n int) {
		l := int64(n)
		if l > rem/3 {
			e.fail(ErrMemoryLimit)
		}
		charge(l * 3)
	}
	for k, vs := range m {
		for _, v := range vs {
			charge3x(len(k))
			charge3x(len(v))
			charge(2) // '=' and '&'
		}
	}
}

func (e *vm) boundedRegexpReplaceAll(re *regexp.Regexp, s, repl string) string {
	if !strings.Contains(repl, "$") {
		e.alloc(int64(len(s)))
		return re.ReplaceAllStringFunc(s, func(match string) string {
			if d := int64(len(repl)) - int64(len(match)); d > 0 {
				e.alloc(d)
			}
			return repl
		})
	}
	numDollars := int64(strings.Count(repl, "$"))
	matches := re.FindAllStringSubmatchIndex(s, -1)
	var out []byte
	last := 0
	for _, m := range matches {
		lit := s[last:m[0]]
		e.alloc(int64(len(lit)))
		out = append(out, lit...)
		rem := e.memLimit - e.memUsed
		matchLen := int64(m[1] - m[0])
		if int64(len(repl)) > rem || (matchLen > 0 && numDollars > (rem-int64(len(repl)))/matchLen) {
			e.fail(ErrMemoryLimit)
		}
		before := len(out)
		out = re.ExpandString(out, repl, s, m)
		if added := int64(len(out) - before); added > 0 {
			e.alloc(added)
		}
		last = m[1]
	}
	tail := s[last:]
	e.alloc(int64(len(tail)))
	out = append(out, tail...)
	return string(out)
}

func (e *vm) callPkg(pkg, fn string, args []any) any {
	need := func(n int) {
		if len(args) != n {
			e.failf("%s.%s expects %d arg(s)", pkg, fn, n)
		}
	}
	switch pkg {
	case "strings":
		switch fn {
		case "Contains", "HasPrefix", "HasSuffix", "Index", "TrimPrefix", "TrimSuffix", "Split", "Join", "Repeat":
			need(2)
			switch fn {
			case "Contains":
				return strings.Contains(e.str(args[0]), e.str(args[1]))
			case "HasPrefix":
				return strings.HasPrefix(e.str(args[0]), e.str(args[1]))
			case "HasSuffix":
				return strings.HasSuffix(e.str(args[0]), e.str(args[1]))
			case "Index":
				return int64(strings.Index(e.str(args[0]), e.str(args[1])))
			case "TrimPrefix":
				return strings.TrimPrefix(e.str(args[0]), e.str(args[1]))
			case "TrimSuffix":
				return strings.TrimSuffix(e.str(args[0]), e.str(args[1]))
			case "Split":
				a, sep := e.str(args[0]), e.str(args[1])
				parts := strings.Split(a, sep)
				e.alloc(int64(len(a)) + int64(len(parts))*16)
				out := make([]any, len(parts))
				for i, p := range parts {
					out[i] = p
				}
				return out
			case "Join":
				ss, sep := e.strSlice(args[0]), e.str(args[1])
				var est int64
				for _, p := range ss {
					est += int64(len(p) + len(sep))
				}
				e.alloc(est)
				return strings.Join(ss, sep)
			case "Repeat":
				s := e.str(args[0])
				return strings.Repeat(s, e.checkRepeat(int64(len(s)), e.int(args[1])))
			}
		case "ToLower", "ToUpper", "TrimSpace":
			need(1)
			s := e.str(args[0])
			if fn == "TrimSpace" {
				return strings.TrimSpace(s)
			}
			e.alloc(int64(len(s)))
			if fn == "ToLower" {
				return strings.ToLower(s)
			}
			return strings.ToUpper(s)
		case "ReplaceAll":
			need(3)
			s, old, repl := e.str(args[0]), e.str(args[1]), e.str(args[2])
			e.checkReplaceAll(int64(len(s)), int64(len(old)), int64(len(repl)), int64(strings.Count(s, old)))
			return strings.ReplaceAll(s, old, repl)
		}
	case "bytes":
		switch fn {
		case "Contains", "HasPrefix", "HasSuffix", "Equal", "Index", "Repeat":
			need(2)
			switch fn {
			case "Contains":
				return bytes.Contains(e.bytes(args[0]), e.bytes(args[1]))
			case "HasPrefix":
				return bytes.HasPrefix(e.bytes(args[0]), e.bytes(args[1]))
			case "HasSuffix":
				return bytes.HasSuffix(e.bytes(args[0]), e.bytes(args[1]))
			case "Equal":
				return bytes.Equal(e.bytes(args[0]), e.bytes(args[1]))
			case "Index":
				return int64(bytes.Index(e.bytes(args[0]), e.bytes(args[1])))
			case "Repeat":
				b := e.bytes(args[0])
				return bytes.Repeat(b, e.checkRepeat(int64(len(b)), e.int(args[1])))
			}
		case "TrimSpace", "ToLower", "ToUpper":
			need(1)
			b := e.bytes(args[0])
			e.alloc(int64(len(b)))
			if fn == "TrimSpace" {
				return append([]byte(nil), bytes.TrimSpace(b)...)
			}
			if fn == "ToLower" {
				return bytes.ToLower(b)
			}
			return bytes.ToUpper(b)
		case "ReplaceAll":
			need(3)
			b, old, repl := e.bytes(args[0]), e.bytes(args[1]), e.bytes(args[2])
			e.checkReplaceAll(int64(len(b)), int64(len(old)), int64(len(repl)), int64(bytes.Count(b, old)))
			return bytes.ReplaceAll(b, old, repl)
		}
	case "encoding/json":
		need(1)
		if fn == "Marshal" {
			e.checkJSONBudget(args[0])
			raw, err := json.Marshal(args[0])
			if err == nil {
				e.alloc(int64(len(raw)))
			}
			return multiVal{raw, err}
		}
		if fn == "Decode" {
			b := e.bytes(args[0])
			e.alloc(int64(len(b)) * 4)
			var dst any
			return multiVal{dst, json.Unmarshal(b, &dst)}
		}
	case "encoding/base64":
		need(1)
		enc := base64.StdEncoding
		if strings.HasPrefix(fn, "URL") {
			enc = base64.RawURLEncoding
		}
		if strings.HasSuffix(fn, "EncodeToString") {
			b := e.bytes(args[0])
			e.alloc(int64(enc.EncodedLen(len(b))))
			return enc.EncodeToString(b)
		}
		if strings.HasSuffix(fn, "DecodeString") {
			s := e.str(args[0])
			e.alloc(int64(len(s)))
			b, err := enc.DecodeString(s)
			return multiVal{b, err}
		}
	case "encoding/hex":
		need(1)
		if fn == "EncodeToString" {
			b := e.bytes(args[0])
			e.alloc(int64(len(b) * 2))
			return hex.EncodeToString(b)
		}
		if fn == "DecodeString" {
			s := e.str(args[0])
			e.alloc(int64(len(s)))
			b, err := hex.DecodeString(s)
			return multiVal{b, err}
		}
	case "net/url":
		need(1)
		switch fn {
		case "QueryEscape":
			out := url.QueryEscape(e.str(args[0]))
			e.alloc(int64(len(out)))
			return out
		case "QueryUnescape":
			out, err := url.QueryUnescape(e.str(args[0]))
			if err == nil {
				e.alloc(int64(len(out)))
			}
			return multiVal{out, err}
		case "ParseQuery":
			s := e.str(args[0])
			e.alloc(int64(len(s)) * 2)
			v, err := url.ParseQuery(s)
			return multiVal{map[string][]string(v), err}
		case "EncodeQuery":
			m, ok := args[0].(map[string][]string)
			if !ok {
				e.failf("url.EncodeQuery expects map[string][]string")
			}
			e.checkQueryBudget(m)
			out := url.Values(m).Encode()
			e.alloc(int64(len(out)))
			return out
		}
	case "regexp":
		if len(args) < 2 {
			e.failf("regexp.%s expects at least 2 args", fn)
		}
		pat, s := e.str(args[0]), e.str(args[1])
		if len(pat) > DefaultMaxRegexBytes {
			e.failf("regexp pattern exceeds %d bytes", DefaultMaxRegexBytes)
		}
		e.alloc(regexpCompileCost + int64(len(pat))*32)
		re, err := regexp.Compile(pat)
		if err != nil {
			e.fail(err)
		}
		switch fn {
		case "MatchString":
			need(2)
			return multiVal{re.MatchString(s), nil}
		case "FindString":
			need(2)
			return re.FindString(s)
		case "ReplaceAllString":
			need(3)
			return e.boundedRegexpReplaceAll(re, s, e.str(args[2]))
		}
	case "crypto/sha256":
		need(1)
		sum := sha256.Sum256(e.bytes(args[0]))
		if fn == "Sum256" {
			return append([]byte(nil), sum[:]...)
		}
		if fn == "Sum256Hex" {
			return hex.EncodeToString(sum[:])
		}
	case "crypto/hmac":
		need(2)
		m := hmac.New(sha256.New, e.bytes(args[0]))
		m.Write(e.bytes(args[1]))
		sum := m.Sum(nil)
		if fn == "SHA256" {
			return sum
		}
		if fn == "SHA256Hex" {
			return hex.EncodeToString(sum)
		}
	case "strconv":
		need(1)
		switch fn {
		case "Atoi":
			n, err := strconv.Atoi(e.str(args[0]))
			return multiVal{int64(n), err}
		case "Itoa":
			return strconv.FormatInt(e.int(args[0]), 10)
		case "ParseBool":
			b, err := strconv.ParseBool(e.str(args[0]))
			return multiVal{b, err}
		case "FormatBool":
			return strconv.FormatBool(e.bool(args[0]))
		}
	case "errors":
		if fn == "New" {
			need(1)
			return errors.New(e.str(args[0]))
		}
	}
	e.failf("unsupported stdlib call %s.%s", pkg, fn)
	return nil
}
