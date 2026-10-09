package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"w103d/gateway/internal/model"
)

// Receipts are never pruned with presentation history. A prepared local create
// reserves its resource ID before the handler can write anything. Recovery can
// therefore inspect that exact ID instead of guessing by a display name.
type operationReceipt struct {
	Format      int         `json:"format"`
	RequestID   string      `json:"requestId"`
	Kind        string      `json:"kind"`
	InputDigest string      `json:"inputDigest"`
	ResourceID  string      `json:"resourceId,omitempty"`
	Status      string      `json:"status"`
	CreatedAt   string      `json:"createdAt"`
	UpdatedAt   string      `json:"updatedAt"`
	ResultCode  int         `json:"resultCode,omitempty"`
	Job         *publishJob `json:"job,omitempty"`
}

type creationIDKey struct{}

func creationResourceID(r *http.Request) string {
	if id, ok := r.Context().Value(creationIDKey{}).(string); ok && validID.MatchString(id) {
		return id
	}
	return NewID() // Direct internal handlers/tests; HTTP routes require a receipt.
}

func (a *App) receiptPath(id string) string {
	return filepath.Join(a.opts.DataDir, "operation-receipts", id+".json")
}

func (a *App) readOperationReceipt(id string) (operationReceipt, error) {
	var receipt operationReceipt
	if !publishRequestID.MatchString(id) {
		return receipt, errors.New("invalid request ID")
	}
	p := a.receiptPath(id)
	info, err := os.Lstat(p)
	if err != nil {
		return receipt, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return receipt, errors.New("invalid operation receipt file")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return receipt, err
	}
	if json.Unmarshal(b, &receipt) != nil || receipt.Format != 1 || receipt.RequestID != id || receipt.Kind == "" || len(receipt.InputDigest) != 64 {
		return receipt, errors.New("invalid operation receipt; original evidence was preserved")
	}
	if _, err := hex.DecodeString(receipt.InputDigest); err != nil || !validID.MatchString(receipt.ResourceID) {
		return receipt, errors.New("invalid receipt identity")
	}
	if receipt.Kind == "publish" {
		if receipt.Job == nil || !validJobStatus(receipt.Status) || receipt.Job.Status != receipt.Status || receipt.Job.RequestID != id || receipt.Job.ID != receipt.ResourceID || receipt.InputDigest != publishReceiptDigest(*receipt.Job) {
			return receipt, errors.New("invalid publish receipt")
		}
	} else {
		switch receipt.Status {
		case "prepared", "succeeded":
		case "rejected":
			if receipt.ResultCode < 400 || receipt.ResultCode >= 500 {
				return receipt, errors.New("invalid rejected receipt")
			}
		default:
			return receipt, errors.New("invalid receipt status")
		}
	}
	return receipt, nil
}

func (a *App) writeOperationReceipt(receipt operationReceipt) error {
	if !publishRequestID.MatchString(receipt.RequestID) {
		return errors.New("invalid request ID")
	}
	receipt.Format = 1
	receipt.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(b) > 2<<20 {
		return errors.New("operation receipt exceeds safe capacity")
	}
	if _, err := os.Lstat(a.receiptPath(receipt.RequestID)); errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(filepath.Dir(a.receiptPath(receipt.RequestID)))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if len(entries) >= 16384 {
			return errors.New("operation receipt capacity reached; preserve receipts and archive the device before accepting more operations")
		}
		var size int64
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".write-") {
				continue
			}
			info, e := entry.Info()
			if e != nil {
				return e
			}
			size += info.Size()
		}
		if size+int64(len(b)) > 64<<20 {
			return errors.New("operation receipt byte capacity reached; no new operation accepted")
		}
	} else if err != nil {
		return err
	}
	return AtomicWrite(a.receiptPath(receipt.RequestID), append(b, '\n'))
}

func (a *App) creationDigest(kind string, body []byte) (string, error) {
	p := filepath.Join(a.opts.DataDir, "operation-receipt.key")
	key, err := readAuthFile(p)
	if errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(filepath.Join(a.opts.DataDir, "operation-receipts"))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".write-") {
				continue
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				return "", errors.New("invalid receipts directory")
			}
			existing, e := a.readOperationReceipt(entry.Name()[:len(entry.Name())-5])
			if e != nil {
				return "", e
			}
			if existing.Kind != "publish" {
				return "", errors.New("receipt key missing; restore the matching key backup instead of generating a replacement")
			}
		}
		key = []byte(randomToken())
		err = AtomicWrite(p, key)
	}
	if err != nil {
		return "", err
	}
	if len(key) < 32 || len(key) > 128 {
		return "", errors.New("invalid receipt authentication key")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func receiptResourceExists(state model.State, id string) bool {
	for _, s := range state.Services {
		if s.ID == id {
			return true
		}
	}
	for _, c := range state.Certificates {
		if c.ID == id {
			return true
		}
	}
	for _, c := range state.DNSAccounts {
		if c.ID == id {
			return true
		}
	}
	for _, c := range state.EdgeOneAccounts {
		if c.ID == id {
			return true
		}
	}
	for _, c := range state.CloudflareAccounts {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (a *App) persistedReceiptState() (model.State, error) {
	var state model.State
	p := filepath.Join(a.opts.DataDir, "state.json")
	info, err := os.Lstat(p)
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return state, errors.New("invalid persisted state for receipt recovery")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(b, &state); err != nil {
		return state, err
	}
	if state.SchemaVersion != model.SchemaVersion {
		return state, errors.New("unsupported persisted state for receipt recovery")
	}
	return state, nil
}

// Called inside changing: acceptance, local creation and receipt finalization
// share its write gate. Only cloud reads occur in these creation handlers.
func (a *App) idempotentCreate(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		if !decode(w, r, &fields) {
			return
		}
		var id string
		if json.Unmarshal(fields["requestId"], &id) != nil || !publishRequestID.MatchString(id) {
			fail(w, 400, "创建操作必须携带稳定 requestId，请刷新管理页面后重试")
			return
		}
		delete(fields, "requestId")
		body, err := json.Marshal(fields)
		if err != nil {
			fail(w, 400, "请求格式不正确")
			return
		}
		kind := r.URL.Path
		inputDigest, err := a.creationDigest(kind, body)
		if err != nil {
			fail(w, 503, "无法读取操作校验密钥，未执行创建")
			return
		}
		receipt, err := a.readOperationReceipt(id)
		if err == nil {
			if receipt.Kind != kind || !hmac.Equal([]byte(receipt.InputDigest), []byte(inputDigest)) {
				fail(w, 409, "同一请求标识不能用于不同输入或操作")
				return
			}
			a.mu.Lock()
			exists := receiptResourceExists(a.state, receipt.ResourceID)
			a.mu.Unlock()
			disk, diskErr := a.persistedReceiptState()
			if diskErr != nil || receiptResourceExists(disk, receipt.ResourceID) != exists {
				fail(w, 503, "创建落盘状态与内存无法核对，请重启管理服务后用原请求查询，未重复创建")
				return
			}
			if receipt.Status == "succeeded" || exists {
				receipt.Status, receipt.ResultCode = "succeeded", 200
				if err = a.writeOperationReceipt(receipt); err != nil {
					fail(w, 503, "创建已完成但收据暂不可保存，请核对原请求")
					return
				}
				w.Header().Set("X-Operation-Resource-ID", receipt.ResourceID)
				a.stateResponse(w)
				return
			}
			if receipt.Status == "rejected" {
				fail(w, receipt.ResultCode, "原创建请求已明确拒绝，请修改后使用新请求标识")
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			fail(w, 503, "无法读取操作收据，未重复创建")
			return
		} else {
			receipt = operationReceipt{RequestID: id, Kind: kind, InputDigest: inputDigest, ResourceID: NewID(), Status: "prepared", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
			if err = a.writeOperationReceipt(receipt); err != nil {
				fail(w, 503, "无法持久保存操作意图，未执行创建")
				return
			}
			if a.pendingCreates == nil {
				a.pendingCreates = map[string]bool{}
			}
			a.pendingCreates[id] = true
		}
		r = r.WithContext(context.WithValue(r.Context(), creationIDKey{}, receipt.ResourceID))
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		result := httptest.NewRecorder()
		handler(result, r)
		receipt.ResultCode = result.Code
		if result.Code >= 200 && result.Code < 300 {
			receipt.Status = "succeeded"
		} else if result.Code >= 400 && result.Code < 500 {
			receipt.Status = "rejected"
		}
		if err = a.writeOperationReceipt(receipt); err != nil {
			fail(w, 503, "操作结果收据保存失败，请使用原请求标识核对，不要重新创建")
			return
		}
		for key, values := range result.Header() {
			w.Header()[key] = values
		}
		w.Header().Set("X-Operation-Resource-ID", receipt.ResourceID)
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	}
}

func publishReceiptDigest(job publishJob) string {
	return digest("publish\x00" + job.ServiceID + "\x00" + job.PublishFingerprint + "\x00" + fmtRevision(job.Revision))
}

func fmtRevision(revision int64) string { b, _ := json.Marshal(revision); return string(b) }

func (a *App) savePublishReceipt(job publishJob) error {
	if existing, err := a.readOperationReceipt(job.RequestID); err == nil {
		if existing.Kind != "publish" || existing.InputDigest != publishReceiptDigest(job) {
			return errors.New("request ID already belongs to another operation")
		}
		if existing.Job != nil && !publishJobActive(existing.Job.Status) && (publishJobActive(job.Status) || job.Status == "interrupted") {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	receipt := operationReceipt{RequestID: job.RequestID, Kind: "publish", InputDigest: publishReceiptDigest(job), ResourceID: job.ID, Status: job.Status, CreatedAt: job.CreatedAt, ResultCode: 202, Job: &job}
	return a.writeOperationReceipt(receipt)
}

// Seal a create that committed state but lost its final receipt before allowing
// deletion or another write. Otherwise deleting that resource could make a
// later retry of its prepared receipt resurrect it.
func (a *App) sealCompletedCreations() error {
	if !a.receiptsInitialized {
		entries, err := os.ReadDir(filepath.Join(a.opts.DataDir, "operation-receipts"))
		if errors.Is(err, os.ErrNotExist) {
			a.pendingCreates = map[string]bool{}
			a.receiptsInitialized = true
			return nil
		}
		if err != nil {
			return err
		}
		a.pendingCreates = map[string]bool{}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				return errors.New("unexpected operation receipt entry")
			}
			id := entry.Name()[:len(entry.Name())-5]
			receipt, err := a.readOperationReceipt(id)
			if err != nil {
				return err
			}
			if receipt.Status == "prepared" {
				a.pendingCreates[id] = true
			}
		}
		a.receiptsInitialized = true
	}
	if len(a.pendingCreates) == 0 {
		return nil
	}
	a.mu.Lock()
	state := clone(a.state)
	a.mu.Unlock()
	disk, err := a.persistedReceiptState()
	if err != nil {
		return err
	}
	for id := range a.pendingCreates {
		receipt, err := a.readOperationReceipt(id)
		if err != nil {
			return err
		}
		if receipt.Status != "prepared" {
			delete(a.pendingCreates, id)
			continue
		}
		if receiptResourceExists(disk, receipt.ResourceID) != receiptResourceExists(state, receipt.ResourceID) {
			return errors.New("persisted creation differs from memory; restart and reconcile before another write")
		}
		if receipt.Status == "prepared" && receiptResourceExists(state, receipt.ResourceID) {
			receipt.Status, receipt.ResultCode = "succeeded", 200
			if err = a.writeOperationReceipt(receipt); err != nil {
				return err
			}
		}
		if receipt.Status != "prepared" {
			delete(a.pendingCreates, id)
		}
	}
	return nil
}

func (a *App) getOperationReceipt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("requestId")
	if !publishRequestID.MatchString(id) {
		fail(w, 400, "请求标识不正确")
		return
	}
	a.jobs.mu.Lock()
	receipt, err := a.readOperationReceipt(id)
	workerAbsent := a.jobs.done == nil
	a.jobs.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		fail(w, 404, "未找到收据；这不证明请求未送达，请使用原请求恢复提交")
		return
	}
	if err != nil {
		fail(w, 503, "操作收据不可读，未执行任何恢复写入")
		return
	}
	if receipt.Job != nil && receipt.Job.Status == "running" && workerAbsent {
		receipt.Status, receipt.Job.Status = "interrupted", "interrupted"
		receipt.Job.Message = "发布执行曾中断，请核对原操作；不会自动重复云端写入"
	}
	if receipt.Status == "prepared" {
		state, err := a.persistedReceiptState()
		if err != nil {
			fail(w, 503, "无法核对原创建的落盘状态")
			return
		}
		exists := receiptResourceExists(state, receipt.ResourceID)
		a.mu.Lock()
		memoryExists := receiptResourceExists(a.state, receipt.ResourceID)
		a.mu.Unlock()
		if exists != memoryExists {
			fail(w, 503, "创建落盘状态与内存不一致，请重启后用原请求核对")
			return
		}
		if exists {
			receipt.Status = "succeeded"
		}
	}
	receipt.InputDigest = "" // Never expose credential-derived comparison material.
	reply(w, 200, receipt)
}
