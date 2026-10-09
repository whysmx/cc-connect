package wecomkf

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// All wecom_kf platform instances (one per [[projects]] entry) that use the
// same listen_addr share a single HTTP server, and those that use the same
// callback_path share one callback route. WeCom delivers every customer
// service account of a corp to the same callback URL, so the route decrypts
// the notification and dispatches it by OpenKfId to the project that owns
// that account.

var hubs = struct {
	sync.Mutex
	m map[string]*hub
}{m: make(map[string]*hub)}

// hub is one shared HTTP listener.
type hub struct {
	addr     string
	listener net.Listener
	server   *http.Server

	mu     sync.RWMutex
	routes map[string]*route // callback_path -> route
}

// route is one shared callback path. Every account on it must use the same
// corp and callback credentials, otherwise notifications could not be
// verified/decrypted unambiguously.
type route struct {
	corpID        string
	callbackToken string
	aesKey        []byte
	accounts      map[string]*Platform // open_kfid -> platform
}

// registerPlatform attaches p to the shared listener/route for its
// listen_addr + callback_path, starting the listener on first use. It fails
// on a duplicate open_kfid or conflicting callback credentials so that a
// message can never be routed to an arbitrary project.
func registerPlatform(p *Platform) error {
	hubs.Lock()
	defer hubs.Unlock()

	h := hubs.m[p.listenAddr]
	if h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		r := h.routes[p.callbackPath]
		if r == nil {
			h.routes[p.callbackPath] = newRoute(p)
			return nil
		}
		if r.corpID != p.corpID || r.callbackToken != p.callbackToken || !bytes.Equal(r.aesKey, p.aesKey) {
			return fmt.Errorf("wecom_kf: callback %s%s is already used with a different corp_id/callback_token/callback_aes_key (project %q)",
				p.listenAddr, p.callbackPath, p.project)
		}
		if other, dup := r.accounts[p.openKfID]; dup {
			return fmt.Errorf("wecom_kf: open_kfid %q is mapped to both project %q and project %q; each customer service account must map to exactly one project",
				p.openKfID, other.project, p.project)
		}
		r.accounts[p.openKfID] = p
		return nil
	}

	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("wecom_kf: listen on %s: %w", p.listenAddr, err)
	}
	h = &hub{
		addr:     p.listenAddr,
		listener: ln,
		routes:   map[string]*route{p.callbackPath: newRoute(p)},
	}
	h.server = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	hubs.m[p.listenAddr] = h
	go func() {
		slog.Info("wecom_kf: callback server listening", "addr", ln.Addr().String())
		if err := h.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("wecom_kf: callback server error", "addr", p.listenAddr, "error", err)
		}
	}()
	return nil
}

func newRoute(p *Platform) *route {
	return &route{
		corpID:        p.corpID,
		callbackToken: p.callbackToken,
		aesKey:        p.aesKey,
		accounts:      map[string]*Platform{p.openKfID: p},
	}
}

// unregisterPlatform detaches p and shuts the listener down once no account
// uses it anymore.
func unregisterPlatform(p *Platform) error {
	hubs.Lock()
	defer hubs.Unlock()
	h := hubs.m[p.listenAddr]
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if r := h.routes[p.callbackPath]; r != nil && r.accounts[p.openKfID] == p {
		delete(r.accounts, p.openKfID)
		if len(r.accounts) == 0 {
			delete(h.routes, p.callbackPath)
		}
	}
	empty := len(h.routes) == 0
	h.mu.Unlock()
	if !empty {
		return nil
	}
	delete(hubs.m, p.listenAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.server.Shutdown(ctx)
}

// lookupHub returns the shared hub for addr (used by tests).
func lookupHub(addr string) *hub {
	hubs.Lock()
	defer hubs.Unlock()
	return hubs.m[addr]
}

func (h *hub) route(path string) *route {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.routes[path]
}

func (h *hub) account(path, openKfID string) *Platform {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if r := h.routes[path]; r != nil {
		return r.accounts[openKfID]
	}
	return nil
}

// ServeHTTP handles WeCom URL verification (GET) and notifications (POST).
func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := h.route(r.URL.Path)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	sig, ts, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")

	switch r.Method {
	case http.MethodGet:
		echo := q.Get("echostr")
		if !verifySignature(rt.callbackToken, ts, nonce, echo, sig) {
			slog.Warn("wecom_kf: URL verification signature mismatch", "path", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		plain, err := decryptPayload(rt.aesKey, echo, rt.corpID)
		if err != nil {
			slog.Warn("wecom_kf: URL verification decrypt failed", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		slog.Info("wecom_kf: callback URL verification succeeded", "path", r.URL.Path)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(plain)

	case http.MethodPost:
		h.handleNotify(w, r, rt, sig, ts, nonce)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *hub) handleNotify(w http.ResponseWriter, r *http.Request, rt *route, sig, ts, nonce string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var env callbackEnvelope
	if err := xml.Unmarshal(body, &env); err != nil || env.Encrypt == "" {
		slog.Warn("wecom_kf: malformed callback body", "bytes", len(body))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !verifySignature(rt.callbackToken, ts, nonce, env.Encrypt, sig) {
		slog.Warn("wecom_kf: callback signature mismatch", "path", r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	plain, err := decryptPayload(rt.aesKey, env.Encrypt, rt.corpID)
	if err != nil {
		slog.Warn("wecom_kf: callback decrypt failed", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ev, err := parseCallbackEvent(plain)
	if err != nil {
		slog.Warn("wecom_kf: callback event parse failed", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Acknowledge immediately; WeCom expects an answer within 5 seconds and
	// the actual messages are pulled asynchronously.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "success")

	if ev.Event != eventKfMsgOrEvent {
		slog.Debug("wecom_kf: ignoring callback event", "event", ev.Event)
		return
	}
	p := h.account(r.URL.Path, ev.OpenKfID)
	if p == nil {
		slog.Warn("wecom_kf: notification for an open_kfid that no project is configured for", "open_kfid", ev.OpenKfID)
		return
	}
	p.triggerSync(ev.Token)
}
