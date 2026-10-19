package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/evidence"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) evidenceStore() *evidence.Store {
	return evidence.New(s.m.pg, s.m.traffic, filepath.Join(s.m.dir, "evidence"))
}

// Add only new optional properties; preserve edited descriptions, existing
// properties, agent bindings and disabled flags. The one-time flag also keeps
// subsequent user unbinding of the evidence reader intact.
func (s *Server) seedFindingTrafficTools() {
	const flag = "finding_traffic_tools_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, key := range []string{"report_finding", "update_finding_report"} {
		var schema any
		if key == "update_finding_report" {
			schema = s.toolUpdateFindingReport().InputSchema()
		} else {
			for _, seed := range agent.BuiltinToolSeeds() {
				if seed.Key == key {
					schema = seed.Schema
					break
				}
			}
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			log.Printf("[evidence] tool schema: %v", err)
			return
		}
		var obj map[string]json.RawMessage
		if err = json.Unmarshal(raw, &obj); err != nil {
			return
		}
		// BuiltinToolSeeds stores Schema as RawMessage; both forms marshal as JSON.
		var properties map[string]json.RawMessage
		if err = json.Unmarshal(obj["properties"], &properties); err != nil {
			return
		}
		name := "traffic_refs"
		if key == "update_finding_report" {
			name = "evidence_version"
		}
		if len(properties[name]) == 0 {
			return
		}
		_, err = s.m.pg.Exec(`UPDATE tools SET schema=jsonb_set(schema,ARRAY['properties',$2::text],$3::jsonb,true),updated_at=now()
WHERE key=$1 AND system AND NOT(COALESCE(schema->'properties','{}'::jsonb) ? $2)`, key, name, string(properties[name]))
		if err != nil {
			log.Printf("[evidence] upgrade tool %s: %v", key, err)
			return
		}
	}
	if reporter, _ := s.m.pg.GetAgentByKey("reporter"); reporter != nil {
		if err := s.m.pg.AddAgentToToolBinding("reporter", []string{"get_finding_traffic"}); err != nil {
			return
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

func (s *Server) registerFindingTraffic(mux *http.ServeMux) {
	base := "/api/exploration/findings/{id}/traffic"
	mux.HandleFunc("GET "+base, s.getFindingTraffic)
	mux.HandleFunc("POST "+base, s.bindFindingTraffic)
	mux.HandleFunc("PATCH "+base+"/{binding_id}", s.editFindingTraffic)
	mux.HandleFunc("DELETE "+base+"/{binding_id}", s.editFindingTraffic)
	mux.HandleFunc("PUT "+base+"/order", s.editFindingTraffic)
	mux.HandleFunc("GET "+base+"/{binding_id}", s.getFindingTrafficDetail)
	mux.HandleFunc("GET "+base+"/{binding_id}/body", s.getFindingTrafficBody)
	mux.HandleFunc("GET "+base+"/{binding_id}/raw", s.getFindingTrafficRaw)
}

func evidenceError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, db.ErrEvidenceConflict) || errors.Is(err, db.ErrTaskArchiveState) {
		status = http.StatusConflict
	}
	if errors.Is(err, db.ErrFindingNotFound) || errors.Is(err, db.ErrEvidenceNotFound) {
		status = http.StatusNotFound
	}
	writeErr(w, status, err.Error())
}

func (s *Server) findingTrafficAccess(w http.ResponseWriter, r *http.Request, write bool) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid finding id")
		return 0, false
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return 0, false
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return 0, false
	}
	// 部门可见性：发现的可见范围跟随其所属任务（与任务列表同一套部门模型）。不可见按
	// 不存在处理，不暴露「存在但无权查看」；写操作还要求对该任务有写权限（部门访客只读）。
	// /traffic、/packets、/packet 这几族接口都从这里进入，校验放在这里即覆盖全部读写分支。
	if write {
		if !s.requireFindingWrite(w, r, id) {
			return 0, false
		}
	} else if !s.requireFindingRead(w, r, id) {
		return 0, false
	}
	if taskID := r.URL.Query().Get("context_task"); taskID != "" {
		task := s.m.ResolveTask(taskID)
		if task == nil {
			writeErr(w, 404, "context task not found")
			return 0, false
		}
		_, inherited, allowed := findingProvenanceInTask(task, f.TaskID)
		if !allowed {
			writeErr(w, 404, "finding not available in task context")
			return 0, false
		}
		if write && inherited {
			writeErr(w, 403, "继承漏洞只读，请在来源任务中修改")
			return 0, false
		}
	}
	return id, true
}

func trafficSummary(in *db.FindingTraffic) *db.FindingTraffic {
	out := *in
	out.Bindings = append([]db.FindingTrafficBinding{}, in.Bindings...)
	for i := range out.Bindings {
		out.Bindings[i].Snapshot.ReqHead = ""
		out.Bindings[i].Snapshot.RespHead = ""
	}
	return &out
}

func (s *Server) getFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	out, err := s.m.pg.GetFindingTraffic(r.Context(), id)
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, trafficSummary(out))
}

func (s *Server) bindFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, 400, "invalid body")
		return
	}
	if len(body.Refs) == 0 {
		writeErr(w, 400, "请选择流量")
		return
	}
	out, err := s.evidenceStore().Bind(r.Context(), id, body.Refs)
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, trafficSummary(out))
}

func (s *Server) editFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Version *int64   `json:"version"`
		Role    *string  `json:"role"`
		Note    *string  `json:"note"`
		Order   []string `json:"binding_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Version == nil {
		writeErr(w, 400, "version 和有效请求体必填")
		return
	}
	var order []int64
	bindingID := int64(0)
	if r.Method == http.MethodPut {
		if body.Order == nil {
			writeErr(w, 400, "binding_ids 必填")
			return
		}
		order = []int64{}
		for _, raw := range body.Order {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v <= 0 {
				writeErr(w, 400, "invalid binding id")
				return
			}
			order = append(order, v)
		}
	} else {
		var err error
		bindingID, err = strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
		if err != nil || bindingID <= 0 {
			writeErr(w, 400, "invalid binding id")
			return
		}
	}
	err := s.m.pg.EditFindingTraffic(r.Context(), id, bindingID, *body.Version, body.Role, body.Note, r.Method == http.MethodDelete, order)
	if err != nil {
		evidenceError(w, err)
		return
	}
	s.getFindingTraffic(w, r)
}

type evidencePreview struct {
	Content    string `json:"content"`
	Offset     int64  `json:"offset"`
	Total      int64  `json:"total"`
	NextOffset int64  `json:"next_offset"`
	Truncated  bool   `json:"truncated"`
	Binary     bool   `json:"binary"`
}

func readEvidencePreview(store *evidence.Store, snapshot db.TrafficEvidenceSnapshot, side string, offset, length int64) (out evidencePreview, err error) {
	if offset < 0 || length < 0 {
		return out, errors.New("offset / length 不能为负数")
	}
	if length == 0 || length > 8192 {
		length = 8192
	}
	f, total, err := store.OpenBody(snapshot, side)
	if err != nil {
		return out, err
	}
	defer f.Close()
	if offset > total {
		return out, errors.New("offset 超出正文长度")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return out, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, length))
	if err != nil {
		return out, err
	}
	// Leave an incomplete trailing UTF-8 rune for the next page. Explicit byte
	// offsets are still accepted; complete downloads always retain original bytes.
	if offset+int64(len(raw)) < total && len(raw) >= utf8.UTFMax {
		start := len(raw) - 1
		for start > 0 && raw[start]&0xc0 == 0x80 {
			start--
		}
		if !utf8.FullRune(raw[start:]) {
			raw = raw[:start]
		}
	}
	out = evidencePreview{Offset: offset, Total: total, NextOffset: offset + int64(len(raw)), Truncated: offset+int64(len(raw)) < total,
		Binary: bytes.IndexByte(raw, 0) >= 0 || (offset == 0 && !utf8.Valid(raw))}
	if out.Binary {
		out.Content = fmt.Sprintf("[二进制正文，%d 字节；请下载查看]", total)
	} else {
		out.Content = string(bytes.ToValidUTF8(raw, []byte("�")))
	}
	return out, nil
}

func (s *Server) getFindingTrafficDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	bid, err := strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
	if err != nil || bid <= 0 {
		writeErr(w, 400, "invalid binding id")
		return
	}
	store := s.evidenceStore()
	var result any
	err = store.WithBinding(r.Context(), id, bid, func(b db.FindingTrafficBinding) error {
		req, err := readEvidencePreview(store, b.Snapshot, "request", 0, 8192)
		if err != nil {
			return err
		}
		resp, err := readEvidencePreview(store, b.Snapshot, "response", 0, 8192)
		if err != nil {
			return err
		}
		result = map[string]any{"binding": b, "request": req, "response": resp}
		return nil
	})
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) getFindingTrafficBody(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	bid, err := strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
	if err != nil || bid <= 0 {
		writeErr(w, 400, "invalid binding id")
		return
	}
	side := r.URL.Query().Get("side")
	store := s.evidenceStore()
	// Resolve the binding under the evidence lock, then read the blob without it:
	// both paths below are O(body size) and would otherwise stall every evidence
	// write for as long as the client takes to receive the data.
	b, err := store.Binding(r.Context(), id, bid)
	if err != nil {
		evidenceError(w, err)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		f, length, err := store.OpenBody(b.Snapshot, side)
		if err != nil {
			evidenceError(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"evidence-%d-%s.bin\"", bid, side))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		_, _ = io.Copy(w, f)
		return
	}
	offset, length := int64(0), int64(8192)
	for name, dst := range map[string]*int64{"offset": &offset, "length": &length} {
		if raw := r.URL.Query().Get(name); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				evidenceError(w, err)
				return
			}
			*dst = v
		}
	}
	preview, err := readEvidencePreview(store, b.Snapshot, side, offset, length)
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, preview)
}

// rawPacketMaxBody caps how much body text is inlined in the JSON raw-packet
// response. Downloads (?download=1) stream the full body instead, so a large
// capture is never silently clipped into a packet someone pastes.
const rawPacketMaxBody = 1 << 20 // 1 MiB

// findingRawPacketDTO is one paste-ready raw HTTP message plus the metadata the
// UI needs to label it (method/URL/status) and to warn about binary or clipped
// bodies.
type findingRawPacketDTO struct {
	Side        string `json:"side"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	CapturedAt  int64  `json:"captured_at"`
	Raw         string `json:"raw"`
	Bytes       int64  `json:"bytes"`      // raw 文本的总字节数（含未内联部分）
	HeadBytes   int64  `json:"head_bytes"` // 起始行 + 头部字节数
	BodyBytes   int64  `json:"body_bytes"` // 正文总字节数
	Binary      bool   `json:"binary"`
	Truncated   bool   `json:"truncated"` // 正文被 rawPacketMaxBody 截断
}

// rawPacketPrefix renders the head part of a paste-ready raw HTTP message: the
// captured head (already LF-terminated) plus the single blank line that separates
// head from body.
func rawPacketPrefix(head string) string {
	if head != "" && !strings.HasSuffix(head, "\n") {
		head += "\n"
	}
	return head + "\n"
}

// assembleRawPacket rebuilds a paste-ready raw HTTP message from a captured head
// and its body.
func assembleRawPacket(head string, body []byte) string {
	return rawPacketPrefix(head) + string(body)
}

// getFindingTrafficRaw returns one bound evidence snapshot as a COMPLETE raw HTTP
// packet (request line + headers + blank line + body), so a human can copy it and
// paste it straight into Repeater/curl to re-test. Bodies beyond
// rawPacketMaxBody are reported as truncated and offered as a download; binary
// bodies are never inlined as text.
func (s *Server) getFindingTrafficRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	bid, err := strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
	if err != nil || bid <= 0 {
		writeErr(w, 400, "invalid binding id")
		return
	}
	side := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("side")))
	if side == "" {
		side = "request"
	}
	if side != "request" && side != "response" {
		writeErr(w, 400, "side 必须为 request 或 response")
		return
	}
	store := s.evidenceStore()
	b, err := store.Binding(r.Context(), id, bid)
	if err != nil {
		evidenceError(w, err)
		return
	}
	head := b.Snapshot.ReqHead
	if side == "response" {
		head = b.Snapshot.RespHead
	}
	f, total, err := store.OpenBody(b.Snapshot, side)
	if err != nil {
		evidenceError(w, err)
		return
	}
	defer f.Close()

	// The packet prefix (head + blank line) is known before reading the body, so a
	// download can advertise an exact Content-Length instead of chunking.
	prefix := rawPacketPrefix(head)

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"finding-%d-%s.http\"", id, side))
		w.Header().Set("Content-Length", strconv.FormatInt(int64(len(prefix))+total, 10))
		if _, err := io.WriteString(w, prefix); err != nil {
			return
		}
		_, _ = io.Copy(w, f)
		return
	}

	body, err := io.ReadAll(io.LimitReader(f, rawPacketMaxBody+1))
	if err != nil {
		evidenceError(w, err)
		return
	}
	truncated := int64(len(body)) > rawPacketMaxBody
	if truncated {
		body = body[:rawPacketMaxBody]
	}
	binary := bytes.IndexByte(body, 0) >= 0 || (len(body) > 0 && !utf8.Valid(body))

	dto := findingRawPacketDTO{
		Side:        side,
		Method:      b.Snapshot.Method,
		URL:         b.Snapshot.URL,
		Status:      b.Snapshot.Status,
		ContentType: b.Snapshot.ContentType,
		CapturedAt:  b.Snapshot.CapturedAt,
		Bytes:       int64(len(prefix)) + total,
		HeadBytes:   int64(len(prefix)),
		BodyBytes:   total,
		Binary:      binary,
		Truncated:   truncated,
	}
	if binary {
		// Inlining raw bytes would corrupt the packet; keep the head readable and
		// tell the reader exactly how to get the real bytes.
		dto.Raw = prefix + fmt.Sprintf("[二进制正文 %d 字节，未内联；请用「下载」获取原始字节]", total)
	} else {
		// 截断时 raw 只含前 rawPacketMaxBody 字节，由 Truncated 标记提示用户改用下载。
		dto.Raw = assembleRawPacket(head, body)
	}
	writeJSON(w, 200, dto)
}

func (s *Server) toolGetFindingTraffic() actool.CoreTool {
	return roTool("get_finding_traffic", "读取漏洞已绑定的真实流量证据，不依赖捕获开关。finding_id 使用 report_finding JSON 返回的独立漏洞记录 ID（不是第一行的探索节点 ID）。先不传 binding_id 获取清单及 version；空清单是正常情况，TCP 等非 HTTP 漏洞或未采集时仍可依据文字/命令证据编写报告，不强制绑定。有绑定时按 binding_id、side(request/response)、offset 分段读取正文。写报告时将读取的 version 作为 evidence_version 传给 update_finding_report，后者 finding_id 仍使用探索节点 ID。",
		objSchema(map[string]any{"finding_id": strParam("独立漏洞记录 ID"), "binding_id": strParam("清单里的绑定 ID，省略则返回清单"), "side": strParam("request 或 response，默认 response"), "offset": map[string]any{"type": "integer"}, "length": map[string]any{"type": "integer"}}, "finding_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				FindingID      json.RawMessage `json:"finding_id"`
				BindingID      json.RawMessage `json:"binding_id"`
				Side           string          `json:"side"`
				Offset, Length int64
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			id, bid := parseProfileID(a.FindingID), parseProfileID(a.BindingID)
			if err := s.agentFindingTrafficAccess(ctx, id, false); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if len(a.BindingID) == 0 {
				list, err := s.m.pg.GetFindingTraffic(ctx, id)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(trafficSummary(list))
			}
			if a.Side == "" {
				a.Side = "response"
			}
			store := s.evidenceStore()
			var result any
			err := store.WithBinding(ctx, id, bid, func(b db.FindingTrafficBinding) error {
				preview, err := readEvidencePreview(store, b.Snapshot, a.Side, a.Offset, a.Length)
				if err != nil {
					return err
				}
				head := b.Snapshot.ReqHead
				if a.Side == "response" {
					head = b.Snapshot.RespHead
				}
				result = map[string]any{"binding_id": strconv.FormatInt(bid, 10), "head": head, "body": preview}
				return nil
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(result)
		})
}
