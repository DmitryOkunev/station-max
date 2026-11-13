package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type ProtoField struct {
	Num   uint64
	Type  uint64
	Bytes []byte
	Var   uint64
}

func readVarint(b []byte, i int) (uint64, int, error) {
	var val uint64
	var shift uint
	for i < len(b) {
		by := b[i]
		i++
		val |= uint64(by&0x7f) << shift
		if by&0x80 == 0 {
			return val, i, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, i, errors.New("varint overflow")
		}
	}
	return 0, i, errors.New("unexpected EOF in varint")
}

func appendVarint(out []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(out, c)
		}
		out = append(out, c|0x80)
	}
}

func ParseProtoFields(b []byte) ([]ProtoField, error) {
	var fields []ProtoField
	i := 0
	for i < len(b) {
		tag, next, err := readVarint(b, i)
		if err != nil {
			return nil, fmt.Errorf("tag at %d: %w", i, err)
		}
		i = next
		fn, wt := tag>>3, tag&7
		switch wt {
		case 2:
			ln, next2, err := readVarint(b, i)
			if err != nil {
				return nil, fmt.Errorf("len at %d: %w", i, err)
			}
			i = next2
			if ln > uint64(len(b)-i) {
				return nil, fmt.Errorf("length %d exceeds buffer at %d", ln, i)
			}
			end := i + int(ln)
			payload := make([]byte, int(ln))
			copy(payload, b[i:end])
			fields = append(fields, ProtoField{Num: fn, Type: wt, Bytes: payload})
			i = end
		case 0:
			v, next2, err := readVarint(b, i)
			if err != nil {
				return nil, fmt.Errorf("varint at %d: %w", i, err)
			}
			fields = append(fields, ProtoField{Num: fn, Type: wt, Var: v})
			i = next2
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d at offset %d", wt, i)
		}
	}
	return fields, nil
}

func EncodeProtoFields(fields []ProtoField) []byte {
	var out []byte
	for _, f := range fields {
		out = appendVarint(out, (f.Num<<3)|f.Type)
		switch f.Type {
		case 2:
			out = appendVarint(out, uint64(len(f.Bytes)))
			out = append(out, f.Bytes...)
		case 0:
			out = appendVarint(out, f.Var)
		}
	}
	return out
}

type UserConfigDoc struct {
	Raw          []byte
	TopFields    []ProtoField
	TargetIdx    int
	SubFields    []ProtoField
	JSONFieldIdx int
	RawJSON      []byte
	Parsed       map[string]any
}

func ParseUserConfigDat(raw []byte, uid string) (*UserConfigDoc, error) {
	top, err := ParseProtoFields(raw)
	if err != nil {
		return nil, fmt.Errorf("parse top protobuf: %w", err)
	}
	for idx, f := range top {
		if f.Num != 1 || f.Type != 2 {
			continue
		}
		sub, err := ParseProtoFields(f.Bytes)
		if err != nil {
			continue
		}
		var foundUID string
		jsonIdx := -1
		for j, sf := range sub {
			if sf.Num == 1 && sf.Type == 2 {
				foundUID = string(sf.Bytes)
			} else if sf.Num == 2 && sf.Type == 2 {
				jsonIdx = j
			}
		}
		if foundUID == uid && jsonIdx >= 0 {
			dec := json.NewDecoder(bytes.NewReader(sub[jsonIdx].Bytes))
			dec.UseNumber()
			var parsed map[string]any
			if err := dec.Decode(&parsed); err != nil {
				return nil, fmt.Errorf("parse UID=%s JSON: %w", uid, err)
			}
			rawCopy := make([]byte, len(raw))
			copy(rawCopy, raw)
			jsonCopy := make([]byte, len(sub[jsonIdx].Bytes))
			copy(jsonCopy, sub[jsonIdx].Bytes)
			return &UserConfigDoc{
				Raw:          rawCopy,
				TopFields:    top,
				TargetIdx:    idx,
				SubFields:    sub,
				JSONFieldIdx: jsonIdx,
				RawJSON:      jsonCopy,
				Parsed:       parsed,
			}, nil
		}
	}
	return nil, fmt.Errorf("UID %q not found in user_config.dat", uid)
}

var uniProxySlotRegex = regexp.MustCompile(`"uniProxyUrl"\s*:\s*"[^"]*"\s*`)

func extractUniProxySlot(js []byte) []byte {
	m := uniProxySlotRegex.Find(js)
	if m == nil {
		return nil
	}
	out := make([]byte, len(m))
	copy(out, m)
	return out
}

func marshalCompactNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func scanStringEnd(b []byte, start int) (int, error) {
	if start >= len(b) || b[start] != '"' {
		return 0, fmt.Errorf("expected string quote at %d", start)
	}
	i := start + 1
	for i < len(b) {
		c := b[i]
		if c == '\\' {
			i += 2
			continue
		}
		if c == '"' {
			return i + 1, nil
		}
		i++
	}
	return 0, errors.New("unterminated JSON string")
}

func scanValueEnd(b []byte, start int) (int, error) {
	start = skipWS(b, start)
	if start >= len(b) {
		return 0, errors.New("unexpected end of JSON value")
	}
	switch b[start] {
	case '"':
		return scanStringEnd(b, start)
	case '{', '[':
		depth := 0
		i := start
		for i < len(b) {
			switch b[i] {
			case '"':
				end, err := scanStringEnd(b, i)
				if err != nil {
					return 0, err
				}
				i = end
			case '{', '[':
				depth++
				i++
			case '}', ']':
				depth--
				i++
				if depth == 0 {
					return i, nil
				}
			default:
				i++
			}
		}
		return 0, errors.New("unterminated JSON container")
	default:
		i := start
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i, nil
			default:
				i++
			}
		}
		return i, nil
	}
}

func findObjectMember(b []byte, objStart, objEnd int, targetKey string) (valStart, valEnd int, found bool, err error) {
	i := skipWS(b, objStart)
	if i >= objEnd || b[i] != '{' {
		return 0, 0, false, fmt.Errorf("expected '{' at %d", i)
	}
	i = skipWS(b, i+1)
	if i < objEnd && b[i] == '}' {
		return 0, 0, false, nil
	}
	for i < objEnd {
		i = skipWS(b, i)
		if i < objEnd && b[i] == '}' {
			return 0, 0, false, nil
		}
		if i >= objEnd || b[i] != '"' {
			return 0, 0, false, fmt.Errorf("expected key string at %d", i)
		}
		kEnd, err := scanStringEnd(b, i)
		if err != nil {
			return 0, 0, false, err
		}
		key, err := strconv.Unquote(string(b[i:kEnd]))
		if err != nil {
			return 0, 0, false, err
		}
		i = skipWS(b, kEnd)
		if i >= objEnd || b[i] != ':' {
			return 0, 0, false, fmt.Errorf("expected ':' at %d", i)
		}
		vStart := skipWS(b, i+1)
		vEnd, err := scanValueEnd(b, vStart)
		if err != nil {
			return 0, 0, false, err
		}
		if key == targetKey {
			return vStart, vEnd, true, nil
		}
		i = skipWS(b, vEnd)
		if i < objEnd && b[i] == ',' {
			i++
			continue
		}
		if i < objEnd && b[i] == '}' {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("expected ',' or '}' at %d", i)
	}
	return 0, 0, false, errors.New("unterminated JSON object")
}

func buildNestedJSON(segs []string, valBytes []byte) ([]byte, error) {
	out := valBytes
	for i := len(segs) - 1; i >= 1; i-- {
		kBytes, err := marshalCompactNoEscape(segs[i])
		if err != nil {
			return nil, err
		}
		out = append([]byte("{"), append(kBytes, append([]byte(":"), append(out, '}')...)...)...)
	}
	return out, nil
}

// PatchJSONBytes surgically updates path inside rawJSON, preserving all untouched spans
// (including exact space-padded uniProxyUrl slots).
func PatchJSONBytes(rawJSON []byte, path []string, value any) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("empty JSON path")
	}
	for _, s := range path {
		if s == "uniProxyUrl" || s == "uniProxyQuicUrl" {
			return nil, errors.New("используйте переключатель потока (Bridge/Stock) для изменения uniProxyUrl")
		}
	}

	valBytes, err := marshalCompactNoEscape(value)
	if err != nil {
		return nil, err
	}

	origSlot := extractUniProxySlot(rawJSON)

	curStart, curEnd := skipWS(rawJSON, 0), len(rawJSON)
	for curEnd > curStart && (rawJSON[curEnd-1] == ' ' || rawJSON[curEnd-1] == '\t' || rawJSON[curEnd-1] == '\n' || rawJSON[curEnd-1] == '\r') {
		curEnd--
	}

	for depth, seg := range path {
		vStart, vEnd, found, err := findObjectMember(rawJSON, curStart, curEnd, seg)
		if err != nil {
			return nil, fmt.Errorf("scan at %v: %w", path[:depth+1], err)
		}
		if !found {
			// Insert new member before closing '}' at curEnd-1
			keyJSON, err := marshalCompactNoEscape(seg)
			if err != nil {
				return nil, err
			}
			nestedVal, err := buildNestedJSON(path[depth:], valBytes)
			if err != nil {
				return nil, err
			}
			closeIdx := curEnd - 1
			for closeIdx > curStart && rawJSON[closeIdx] != '}' {
				closeIdx--
			}
			if closeIdx <= curStart || rawJSON[closeIdx] != '}' {
				return nil, errors.New("missing closing brace on insert")
			}
			prevNonWS := closeIdx - 1
			for prevNonWS > curStart && (rawJSON[prevNonWS] == ' ' || rawJSON[prevNonWS] == '\t' || rawJSON[prevNonWS] == '\n' || rawJSON[prevNonWS] == '\r') {
				prevNonWS--
			}
			var insert []byte
			if rawJSON[prevNonWS] != '{' {
				insert = append(insert, ',')
			}
			insert = append(insert, keyJSON...)
			insert = append(insert, ':')
			insert = append(insert, nestedVal...)

			out := make([]byte, 0, len(rawJSON)+len(insert))
			out = append(out, rawJSON[:closeIdx]...)
			out = append(out, insert...)
			out = append(out, rawJSON[closeIdx:]...)
			if origSlot != nil && !bytes.Equal(origSlot, extractUniProxySlot(out)) {
				return nil, errors.New("internal error: uniProxyUrl slot altered during insert")
			}
			return out, nil
		}

		if depth == len(path)-1 {
			out := make([]byte, 0, len(rawJSON)-(vEnd-vStart)+len(valBytes))
			out = append(out, rawJSON[:vStart]...)
			out = append(out, valBytes...)
			out = append(out, rawJSON[vEnd:]...)
			if origSlot != nil && !bytes.Equal(origSlot, extractUniProxySlot(out)) {
				return nil, errors.New("internal error: uniProxyUrl slot altered during replace")
			}
			return out, nil
		}

		if rawJSON[vStart] != '{' {
			return nil, fmt.Errorf("путь %q не является объектом JSON", strings.Join(path[:depth+1], "."))
		}
		curStart, curEnd = vStart, vEnd
	}
	return rawJSON, nil
}

// ResolveDotPath resolves a user-supplied dotted string against parsed JSON so keys
// containing literal dots (e.g. "com.yandex.io.sdk") are kept as single segments.
func ResolveDotPath(parsed map[string]any, dotInput string) []string {
	dotInput = strings.TrimSpace(dotInput)
	if dotInput == "" {
		return nil
	}
	var segs []string
	cur := parsed
	rem := dotInput
	for rem != "" {
		if cur != nil {
			if _, ok := cur[rem]; ok {
				segs = append(segs, rem)
				break
			}
			bestKey := ""
			for k := range cur {
				if strings.HasPrefix(rem, k+".") && len(k) > len(bestKey) {
					bestKey = k
				}
			}
			if bestKey != "" {
				segs = append(segs, bestKey)
				rem = rem[len(bestKey)+1:]
				if nextObj, ok := cur[bestKey].(map[string]any); ok {
					cur = nextObj
				} else {
					cur = nil
				}
				continue
			}
		}
		first, rest, hasDot := strings.Cut(rem, ".")
		segs = append(segs, first)
		if !hasDot {
			break
		}
		rem = rest
		if cur != nil {
			if nextObj, ok := cur[first].(map[string]any); ok {
				cur = nextObj
			} else {
				cur = nil
			}
		}
	}
	return segs
}

func (doc *UserConfigDoc) ApplyChanges(changes []StagedChange) ([]byte, error) {
	curJSON := make([]byte, len(doc.RawJSON))
	copy(curJSON, doc.RawJSON)
	for _, ch := range changes {
		var val any
		dec := json.NewDecoder(bytes.NewReader(ch.Value))
		dec.UseNumber()
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("decode staged %v: %w", ch.Path, err)
		}
		var err error
		curJSON, err = PatchJSONBytes(curJSON, ch.Path, val)
		if err != nil {
			return nil, err
		}
	}

	// Verify resulting JSON is valid
	dec := json.NewDecoder(bytes.NewReader(curJSON))
	dec.UseNumber()
	var check map[string]any
	if err := dec.Decode(&check); err != nil {
		return nil, fmt.Errorf("patched JSON invalid: %w", err)
	}

	newSub := make([]ProtoField, len(doc.SubFields))
	copy(newSub, doc.SubFields)
	newSub[doc.JSONFieldIdx].Bytes = curJSON
	subBytes := EncodeProtoFields(newSub)

	newTop := make([]ProtoField, len(doc.TopFields))
	copy(newTop, doc.TopFields)
	newTop[doc.TargetIdx].Bytes = subBytes
	out := EncodeProtoFields(newTop)

	// Full round-trip verify
	if _, err := ParseUserConfigDat(out, doc.SubFields[0].BytesString()); err != nil {
		return nil, fmt.Errorf("round-trip verify failed: %w", err)
	}
	return out, nil
}

func (f ProtoField) BytesString() string {
	return string(f.Bytes)
}

type StagedChange struct {
	Path  []string        `json:"path"`
	Value json.RawMessage `json:"value"`
}

type DraftStore struct {
	mu      sync.Mutex
	path    string
	Changes []StagedChange `json:"changes"`
}

func NewDraftStore(path string) *DraftStore {
	ds := &DraftStore{path: path}
	_ = ds.load()
	return ds
}

func (ds *DraftStore) load() error {
	raw, err := os.ReadFile(ds.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &ds.Changes)
}

func (ds *DraftStore) saveLocked() error {
	if ds.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(ds.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ds.Changes, "", "  ")
	if err != nil {
		return err
	}
	tmp := ds.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, ds.path)
}

func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (ds *DraftStore) Set(path []string, val any) error {
	raw, err := marshalCompactNoEscape(val)
	if err != nil {
		return err
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for i := range ds.Changes {
		if pathEqual(ds.Changes[i].Path, path) {
			ds.Changes[i].Value = json.RawMessage(raw)
			return ds.saveLocked()
		}
	}
	pathCopy := append([]string(nil), path...)
	ds.Changes = append(ds.Changes, StagedChange{Path: pathCopy, Value: json.RawMessage(raw)})
	return ds.saveLocked()
}

func (ds *DraftStore) RemovePrefix(prefix []string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	var kept []StagedChange
	for _, ch := range ds.Changes {
		if len(ch.Path) >= len(prefix) && pathEqual(ch.Path[:len(prefix)], prefix) {
			continue
		}
		kept = append(kept, ch)
	}
	ds.Changes = kept
	return ds.saveLocked()
}

func (ds *DraftStore) Clear() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.Changes = nil
	return ds.saveLocked()
}

func (ds *DraftStore) Snapshot() []StagedChange {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]StagedChange, len(ds.Changes))
	for i, c := range ds.Changes {
		out[i] = StagedChange{
			Path:  append([]string(nil), c.Path...),
			Value: append(json.RawMessage(nil), c.Value...),
		}
	}
	return out
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = deepCopyMap(sub)
		} else {
			out[k] = v
		}
	}
	return out
}

func setMapPath(m map[string]any, path []string, val any) {
	if len(path) == 0 {
		return
	}
	cur := m
	for _, seg := range path[:len(path)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = make(map[string]any)
			cur[seg] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = val
}

func (ds *DraftStore) Overlay(base map[string]any) (merged map[string]any, dirtyPaths map[string]bool) {
	merged = deepCopyMap(base)
	dirtyPaths = make(map[string]bool)
	for _, ch := range ds.Snapshot() {
		var val any
		dec := json.NewDecoder(bytes.NewReader(ch.Value))
		dec.UseNumber()
		if err := dec.Decode(&val); err != nil {
			continue
		}
		setMapPath(merged, ch.Path, val)
		dirtyPaths[strings.Join(ch.Path, ".")] = true
	}
	return merged, dirtyPaths
}

func GetAtPath(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, seg := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func GetBool(m map[string]any, path ...string) (bool, bool) {
	v, ok := GetAtPath(m, path...)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func GetString(m map[string]any, path ...string) (string, bool) {
	v, ok := GetAtPath(m, path...)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func GetFloat(m map[string]any, path ...string) (float64, bool) {
	v, ok := GetAtPath(m, path...)
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func ParseUserValue(raw string) any {
	s := strings.TrimSpace(raw)
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") || (len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"') {
		var v any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			return v
		}
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return json.Number(s)
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return json.Number(s)
	}
	return s
}

