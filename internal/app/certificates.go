package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"w103d/gateway/internal/atomicfile"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (a *App) createCertificate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		fail(w, 400, "请输入不超过100字节的证书名称")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Certificates) >= 50 {
		fail(w, 400, "证书槽已达上限")
		return
	}
	id := creationResourceID(r)
	inbox := filepath.Join(a.opts.DataDir, "cert-inbox", id)
	if e := os.MkdirAll(inbox, 0700); e != nil {
		fail(w, 500, "无法创建证书暂存目录")
		return
	}
	old := clone(a.state)
	a.state.Certificates = append(a.state.Certificates, model.Certificate{ID: id, Name: in.Name, Domains: []string{}, InboxPath: inbox})
	if e := a.saveLocked(); e != nil {
		a.state = old
		fail(w, 500, "无法保存证书槽")
		return
	}
	a.replyStateLocked(w)
}

func readUpload(r *http.Request, field string) ([]byte, error) {
	f, _, e := r.FormFile(field)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if len(b) > 2<<20 {
		return nil, errors.New("证书文件超过2 MiB")
	}
	return b, e
}

func (a *App) importCertificate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	if e := r.ParseMultipartForm(5 << 20); e != nil {
		fail(w, 400, "证书上传格式错误或文件过大")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	cert, e := readUpload(r, "certificate")
	if e != nil {
		fail(w, 400, "缺少有效的 fullchain PEM 文件")
		return
	}
	key, e := readUpload(r, "privateKey")
	if e != nil {
		fail(w, 400, "缺少有效的 PEM 私钥")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e = a.activateCertificateLocked(r.PathValue("id"), cert, key); e != nil {
		fail(w, 400, e.Error())
		return
	}
	a.replyStateLocked(w)
}

func (a *App) activateCertificateLocked(id string, cert, key []byte) error {
	if !validID.MatchString(id) {
		return errors.New("invalid certificate ID")
	}
	index := a.certIndex(id)
	if index < 0 {
		return errors.New("certificate slot not found")
	}
	oldCert := a.state.Certificates[index]
	parsed, err := engine.ParseCertificate(id, oldCert.Name, cert, key)
	if err != nil {
		return err
	}
	parsed.InboxPath, parsed.EdgeOne = oldCert.InboxPath, oldCert.EdgeOne
	if dir, e := engine.CertificateDirectory(a.opts.DataDir, oldCert); e == nil {
		oldPEM, e1 := os.ReadFile(filepath.Join(dir, "fullchain.pem"))
		oldKey, e2 := os.ReadFile(filepath.Join(dir, "privkey.pem"))
		if e1 == nil && e2 == nil && oldCert.Ready && oldCert.Fingerprint == parsed.Fingerprint && bytes.Equal(oldPEM, cert) && bytes.Equal(oldKey, key) {
			return nil
		}
	}
	applied, err := AppliedState(a.opts)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	active := false
	for _, service := range applied.Services {
		if !service.Enabled || !service.UsesDomains() || service.CertificateID != id {
			continue
		}
		active = true
		for _, domain := range service.Domains {
			if err := leaf.VerifyHostname(domain); err != nil {
				return fmt.Errorf("certificate does not cover running domain %s", domain)
			}
		}
	}
	// Future draft domains are checked by normal publish validation, not renewal.
	if err := storeCertificateVersion(a.opts.DataDir, &parsed, cert, key); err != nil {
		return err
	}
	nextDraft := clone(a.state)
	nextDraft.Certificates[index] = parsed
	if !active {
		if err := writeRuntimeJSON(filepath.Join(a.opts.DataDir, "state.json"), nextDraft); err != nil {
			if atomicfile.IsCommitted(err) {
				a.state = nextDraft
				a.beginShutdown()
			}
			return err
		}
		a.state = nextDraft
		return nil
	}
	nextApplied := clone(applied)
	for i := range nextApplied.Certificates {
		if nextApplied.Certificates[i].ID == id {
			nextApplied.Certificates[i] = parsed
		}
	}
	if err := a.commitRuntimeLocked(nextDraft, nextApplied, writeRuntimeJSON); err != nil {
		return err
	}
	a.propagateCertificate(id)
	return nil
}
func (a *App) deleteCertificate(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	i := a.certIndex(id)
	if i < 0 {
		fail(w, 404, "证书槽不存在")
		return
	}
	applied, e := AppliedState(a.opts)
	if e != nil {
		fail(w, 500, "无法读取已应用配置")
		return
	}
	for _, service := range append(append([]model.Service{}, a.state.Services...), applied.Services...) {
		if service.CertificateID == id {
			fail(w, 409, "证书仍被草稿或已应用的服务引用，请先解除引用并应用")
			return
		}
	}
	old := clone(a.state)
	a.state.Certificates = append(a.state.Certificates[:i], a.state.Certificates[i+1:]...)
	if e = a.saveLocked(); e != nil {
		a.state = old
		fail(w, 500, "无法保存配置")
		return
	}
	// Certificate bytes are retained for recovery; only the managed slot is removed.
	a.replyStateLocked(w)
}

func (a *App) startControl() (func(context.Context) error, error) {
	socket := filepath.Join(a.opts.DataDir, "control.sock")
	if st, e := os.Lstat(socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("控制套接字路径被非套接字文件占用")
		}
		c, e := net.DialTimeout("unix", socket, time.Second)
		if e == nil {
			c.Close()
			return nil, errors.New("已有另一个网关进程在运行")
		}
		if e = os.Remove(socket); e != nil {
			return nil, e
		}
	}
	listener, e := net.Listen("unix", socket)
	if e != nil {
		return nil, e
	}
	if e = os.Chmod(socket, 0600); e != nil {
		listener.Close()
		return nil, e
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/certificates/{id}/activate", func(w http.ResponseWriter, r *http.Request) {
		_, finish, accepted := a.beginWork()
		if !accepted {
			w.Header().Set("Retry-After", "5")
			fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
			return
		}
		defer finish()
		if !a.changeMu.TryLock() {
			w.Header().Set("Retry-After", "30")
			fail(w, 503, "网关正在发布，请稍后重试证书激活；收件材料已保留")
			return
		}
		defer a.changeMu.Unlock()
		id := r.PathValue("id")
		if !validID.MatchString(id) {
			fail(w, 400, "无效证书ID")
			return
		}
		if !lockContext(r.Context(), &a.mu) {
			fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
			return
		}
		defer a.mu.Unlock()
		i := a.certIndex(id)
		if i < 0 {
			fail(w, 404, "证书槽不存在")
			return
		}
		dir := filepath.Join(a.opts.DataDir, "cert-inbox", id)
		cert, e := readLimitedFile(filepath.Join(dir, "fullchain.pem"))
		if e != nil {
			fail(w, 400, "无法读取暂存证书")
			return
		}
		key, e := readLimitedFile(filepath.Join(dir, "privkey.pem"))
		if e != nil {
			fail(w, 400, "无法读取暂存私钥")
			return
		}
		if e = a.activateCertificateLocked(id, cert, key); e != nil {
			fail(w, 400, e.Error())
			return
		}
		reply(w, 200, map[string]any{"ok": true, "fingerprint": a.state.Certificates[i].Fingerprint})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second, MaxHeaderBytes: 4096,
		BaseContext: func(net.Listener) context.Context { return a.lifecycleContext() }}
	go func() { _ = server.Serve(listener) }()
	return func(ctx context.Context) error {
		err := server.Shutdown(ctx)
		if err != nil {
			_ = server.Close()
		}
		_ = os.Remove(socket)
		return err
	}, nil
}

func ActivateViaSocket(dataDir, id string) error {
	if !validID.MatchString(id) {
		return errors.New("证书槽位 ID 不合法")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", filepath.Join(dataDir, "control.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	r, e := client.Post("http://gateway/internal/certificates/"+id+"/activate", "application/json", strings.NewReader("{}"))
	if e != nil {
		return fmt.Errorf("网关未在运行：%w", e)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 65536))
	if r.StatusCode != 200 {
		var result struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &result)
		return fmt.Errorf("证书应用失败：%s", result.Error)
	}
	fmt.Println(string(b))
	return nil
}
