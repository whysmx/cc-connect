package wecomkf

import (
	"log/slog"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// defaultMergeWindow matches the private-chat aggregation of the wecom
// WebSocket adapter: WeChat customers often send a file or picture and then
// the question about it as a separate message.
const defaultMergeWindow = 2 * time.Second

// aggregate collects one customer's messages that arrive within the merge
// window so the agent gets a single turn (and the reply budget is not split
// across several answers).
type aggregate struct {
	msg     *core.Message
	timer   *time.Timer
	pending int  // media downloads still in flight
	due     bool // window elapsed while downloads were pending
}

// emit hands a ready message (text) to the engine, merging it with other
// messages of the same customer when a merge window is configured.
func (p *Platform) emit(msg *core.Message) {
	if p.mergeWindow <= 0 {
		go p.handler(p, msg)
		return
	}
	p.aggMu.Lock()
	a := p.aggregateLocked(msg)
	mergeInto(a.msg, msg)
	a.timer.Reset(p.mergeWindow)
	p.aggMu.Unlock()
}

// reserve announces a media message whose download has started.
func (p *Platform) reserve(msg *core.Message) {
	if p.mergeWindow <= 0 {
		return
	}
	p.aggMu.Lock()
	a := p.aggregateLocked(msg)
	a.pending++
	a.timer.Reset(p.mergeWindow)
	p.aggMu.Unlock()
}

// complete finishes a reserved media message; msg is nil when the download
// failed.
func (p *Platform) complete(sessionKey string, msg *core.Message) {
	if p.mergeWindow <= 0 {
		if msg != nil {
			p.handler(p, msg)
		}
		return
	}
	p.aggMu.Lock()
	a := p.agg[sessionKey]
	if a == nil { // dropped by Stop
		p.aggMu.Unlock()
		return
	}
	if msg != nil {
		mergeInto(a.msg, msg)
	}
	a.pending--
	flushNow := a.due && a.pending == 0
	if flushNow {
		delete(p.agg, sessionKey)
	} else if a.pending == 0 {
		a.timer.Reset(p.mergeWindow)
	}
	p.aggMu.Unlock()
	if flushNow {
		p.dispatch(a.msg)
	}
}

// aggregateLocked returns the customer's open aggregate, creating it (with
// the routing fields of msg and empty content) if needed. aggMu must be held.
func (p *Platform) aggregateLocked(msg *core.Message) *aggregate {
	if a := p.agg[msg.SessionKey]; a != nil {
		return a
	}
	base := *msg
	base.Content, base.Images, base.Files, base.Audio = "", nil, nil, nil
	a := &aggregate{msg: &base}
	key := msg.SessionKey
	a.timer = time.AfterFunc(time.Hour, func() { p.flush(key, a) })
	p.agg[key] = a
	return a
}

// flush runs when the merge window elapses.
func (p *Platform) flush(key string, a *aggregate) {
	p.aggMu.Lock()
	if p.agg[key] != a {
		p.aggMu.Unlock()
		return
	}
	if a.pending > 0 {
		a.due = true
		p.aggMu.Unlock()
		return
	}
	delete(p.agg, key)
	p.aggMu.Unlock()
	p.dispatch(a.msg)
}

// dispatch sends a merged message unless every part of it failed.
func (p *Platform) dispatch(msg *core.Message) {
	if msg.Content == "" && len(msg.Images) == 0 && len(msg.Files) == 0 && msg.Audio == nil {
		return
	}
	p.handler(p, msg)
}

// dropAggregates discards undispatched messages on shutdown.
func (p *Platform) dropAggregates() {
	p.aggMu.Lock()
	defer p.aggMu.Unlock()
	for key, a := range p.agg {
		a.timer.Stop()
		delete(p.agg, key)
		slog.Warn("wecom_kf: dropping unsent customer message on shutdown", "session", key)
	}
}

// mergeInto appends src's content and attachments to dst and makes dst
// reply to the most recent message.
func mergeInto(dst, src *core.Message) {
	if src.Content != "" {
		dst.Content = strings.TrimSpace(strings.Join([]string{dst.Content, src.Content}, "\n"))
	}
	dst.Images = append(dst.Images, src.Images...)
	dst.Files = append(dst.Files, src.Files...)
	if src.Audio != nil {
		dst.Audio = src.Audio
	}
	dst.MessageID, dst.ReplyCtx = src.MessageID, src.ReplyCtx
	if src.UserMessageTimeMs > dst.UserMessageTimeMs {
		dst.UserMessageTimeMs = src.UserMessageTimeMs
	}
}
