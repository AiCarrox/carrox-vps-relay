// Panel self-hosting: once an admin has saved a Cloudflare token, issue a
// certificate for the panel's own domain and switch its vhost to HTTPS.
//
// The panel domain is written by deploy/install.sh (--domain) to
// /etc/vps-relay/panel-domain. Without that file nothing here runs.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	panelDomainPath    = "/etc/vps-relay/panel-domain"
	panelCertOwnerPath = "/etc/vps-relay/panel-cert-owner"
	panelConfPath      = confDir + "/relay-panel.conf"
	panelSSLTemplate   = "/opt/vps-relay/templates/panel-ssl.conf.tmpl"
)

var (
	panelMu      sync.Mutex
	panelBusy    bool
	panelLastErr string
)

type panelStatus struct {
	Domain string `json:"domain"`
	HTTPS  bool   `json:"https"`
	// none: no panel domain; http: waiting for an admin CF token;
	// issuing: certificate in progress; https: done; error: see Error.
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
	MCPURL string `json:"mcp_url,omitempty"`
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func panelDomain() string {
	d := strings.ToLower(readTrimmed(panelDomainPath))
	if d == "" || validateDomain(d) != nil {
		return ""
	}
	return d
}

func panelIsHTTPS() bool {
	b, err := os.ReadFile(panelConfPath)
	return err == nil && strings.Contains(string(b), "ssl_certificate")
}

func getPanelStatus() panelStatus {
	st := panelStatus{Domain: panelDomain(), State: "none"}
	if st.Domain == "" {
		return st
	}
	panelMu.Lock()
	busy, lastErr := panelBusy, panelLastErr
	panelMu.Unlock()
	switch {
	case panelIsHTTPS():
		st.HTTPS = true
		st.State = "https"
		st.MCPURL = "https://" + st.Domain + "/mcp"
	case busy:
		st.State = "issuing"
	case lastErr != "":
		st.State = "error"
		st.Error = lastErr
	default:
		st.State = "http"
	}
	return st
}

func panelInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, getPanelStatus())
}

// panelHTTPSPending reports whether ensurePanelHTTPS would have work to do.
func panelHTTPSPending() bool {
	if panelDomain() == "" || panelIsHTTPS() {
		return false
	}
	_, err := os.Stat(panelConfPath)
	return err == nil
}

// ensurePanelHTTPS is safe to call repeatedly and from multiple goroutines;
// it is a no-op unless install.sh set up an HTTP-only panel vhost.
func ensurePanelHTTPS() {
	if !panelHTTPSPending() {
		return
	}
	panelMu.Lock()
	if panelBusy {
		panelMu.Unlock()
		return
	}
	panelBusy = true
	panelLastErr = ""
	panelMu.Unlock()

	domain := panelDomain()
	err := switchPanelToHTTPS(domain)

	panelMu.Lock()
	panelBusy = false
	if err != nil {
		panelLastErr = err.Error()
	}
	panelMu.Unlock()

	if err != nil {
		log.Printf("[panel-https] %s: %v", domain, err)
	}
}

func adminKeyIDs() []string {
	keyTableMu.RLock()
	defer keyTableMu.RUnlock()
	var ids []string
	for _, e := range keyTable {
		if e.Role == "admin" {
			ids = append(ids, e.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// pickPanelAdmin returns the first admin whose CF token can manage the panel
// domain's zone. haveToken reports whether any admin has a token at all.
func pickPanelAdmin(domain string) (owner, tok string, haveToken bool) {
	for _, id := range adminKeyIDs() {
		t := cfTokenForKey(id)
		if t == "" {
			continue
		}
		haveToken = true
		if _, err := cfPickZoneFor(t, domain); err == nil {
			return id, t, true
		}
	}
	return "", "", haveToken
}

func switchPanelToHTTPS(domain string) error {
	owner, tok, haveToken := pickPanelAdmin(domain)
	if !haveToken {
		return nil // nothing to do until an admin saves a CF token
	}
	if owner == "" {
		return fmt.Errorf("没有管理员的 Cloudflare Token 能管理面板域名 %s 所在的 Zone，面板保持 HTTP", domain)
	}

	// Reuse a still-valid certificate (e.g. left by an earlier attempt).
	if _, days, err := certInfo(domain); err != nil || days <= renewThresholdDays {
		log.Printf("[panel-https] %s: issuing certificate (owner %s)…", domain, owner)
		if err := acmeIssue(domain, tok, owner); err != nil {
			return fmt.Errorf("签发面板证书失败: %w", err)
		}
		if err := atomicWrite(panelCertOwnerPath, []byte(owner+"\n"), 0o600); err != nil {
			log.Printf("[panel-https] record cert owner: %v", err)
		}
	}

	tmpl, err := os.ReadFile(panelSSLTemplate)
	if err != nil {
		return fmt.Errorf("读取面板模板失败: %w", err)
	}
	rendered := []byte(strings.NewReplacer(
		"{{.Domain}}", domain,
		"{{.HTTPSListen}}", httpsListen(),
	).Replace(string(tmpl)))

	old, err := os.ReadFile(panelConfPath)
	if err != nil {
		return fmt.Errorf("读取面板配置失败: %w", err)
	}
	if err := atomicWrite(panelConfPath, rendered, 0o644); err != nil {
		return fmt.Errorf("写入面板配置失败: %w", err)
	}
	if err := nginxSyntaxCheck(); err != nil {
		_ = atomicWrite(panelConfPath, old, 0o644)
		return err
	}
	if err := nginxReload(); err != nil {
		_ = atomicWrite(panelConfPath, old, 0o644)
		return errors.New("nginx reload 失败: " + err.Error())
	}
	log.Printf("[panel-https] %s: panel switched to HTTPS", domain)
	return nil
}

// renewPanelCert keeps the HTTPS panel's certificate fresh. It also takes over
// a certificate that was installed by other means (no recorded owner), using
// the first admin whose CF token can manage the panel domain.
func renewPanelCert() {
	domain := panelDomain()
	if domain == "" || !panelIsHTTPS() {
		return
	}
	_, days, err := certInfo(domain)
	if err != nil || days > renewThresholdDays {
		return
	}
	owner := readTrimmed(panelCertOwnerPath)
	tok := ""
	if owner != "" {
		tok = cfTokenForKey(owner)
	}
	if tok == "" {
		owner, tok, _ = pickPanelAdmin(domain)
	}
	if tok == "" {
		log.Printf("[auto-renew] panel %s: %d days left but no admin CF token can manage its zone", domain, days)
		return
	}
	log.Printf("[auto-renew] panel %s: %d days left, renewing (owner %s)…", domain, days, owner)
	if err := acmeRenewOrIssue(domain, tok, owner); err != nil {
		log.Printf("[auto-renew] panel %s: renew failed: %v", domain, err)
		return
	}
	if err := atomicWrite(panelCertOwnerPath, []byte(owner+"\n"), 0o600); err != nil {
		log.Printf("[auto-renew] panel %s: record cert owner: %v", domain, err)
	}
	log.Printf("[auto-renew] panel %s: renewed OK", domain)
}
