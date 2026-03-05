// Package telegram — Telegram Bot long-poll plugin.
// Reply routing: subscribes to bus.ReplyKey("telegram"), reads tg_chat_id from Meta.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/core"
	"github.com/krill/krill/internal/ingress"
)

func init() {
	core.Global().RegisterProtocol("telegram", func(cfg map[string]interface{}) (core.Protocol, error) {
		return New(cfg)
	})
}

const tgAPI = "https://api.telegram.org/bot"

type Plugin struct {
	token  string
	pollMs time.Duration
	b      bus.Bus
	log    *slog.Logger
	cancel context.CancelFunc
	http   *http.Client
	norm   *ingress.Normalizer
}

func New(cfg map[string]interface{}) (*Plugin, error) {
	token, _ := cfg["token"].(string)
	if token == "" {
		return nil, fmt.Errorf("telegram: token required")
	}
	ms := 1000
	if v, ok := cfg["poll_ms"].(int); ok && v > 0 {
		ms = v
	}
	return &Plugin{
		token:  token,
		pollMs: time.Duration(ms) * time.Millisecond,
		http:   &http.Client{Timeout: 35 * time.Second},
		norm:   ingress.NewNormalizer(boolVal(cfg, "_strict_v2_validation") || boolVal(cfg, "strict_v2_validation")),
	}, nil
}

func (p *Plugin) Name() string { return "telegram" }

func (p *Plugin) Start(ctx context.Context, b bus.Bus, log *slog.Logger) error {
	p.b = b
	p.log = log
	pollCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	go p.poll(pollCtx)
	go p.relay(pollCtx) // relay replies back to Telegram users
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

// poll fetches Telegram updates and publishes them to the inbound bus.
func (p *Plugin) poll(ctx context.Context) {
	offset := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.pollMs):
		}
		updates, err := p.getUpdates(ctx, offset)
		if err != nil {
			p.log.Warn("telegram poll", "err", err)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			chatID := strconv.Itoa(u.Message.Chat.ID)
			clientID := "tg:" + chatID
			env := &bus.Envelope{
				ID:             uuid.NewString(),
				ClientID:       clientID,
				ThreadID:       clientID,
				Role:           bus.RoleUser,
				Text:           u.Message.Text,
				SourceProtocol: "telegram",
				Meta: map[string]string{
					"tg_chat_id": chatID,
					"tg_user":    u.Message.From.Username,
				},
				CreatedAt: time.Now(),
			}
			p.norm.PublishInbound(ctx, p.b, env) //nolint:errcheck
		}
	}
}

// relay listens for assistant replies and sends them to the Telegram chat.
func (p *Plugin) relay(ctx context.Context) {
	ch := p.b.Subscribe(bus.ReplyKey("telegram"))
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-ch:
			if !ok {
				return
			}
			if env.Role != bus.RoleAssistant {
				continue
			}
			chatID := env.Meta["tg_chat_id"]
			if chatID == "" {
				// Fallback: extract from clientID "tg:<chat_id>"
				if len(env.ClientID) > 3 {
					chatID = env.ClientID[3:]
				}
			}
			if chatID == "" {
				continue
			}
			if err := p.send(ctx, chatID, env.Text); err != nil {
				p.log.Warn("telegram send", "err", err, "chat", chatID)
			}
		}
	}
}

// ─── Telegram API ─────────────────────────────────────────────────────────────

type tgUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID int `json:"id"`
		} `json:"chat"`
		From struct {
			Username string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

func (p *Plugin) getUpdates(ctx context.Context, offset int) ([]tgUpdate, error) {
	url := fmt.Sprintf("%s%s/getUpdates?offset=%d&timeout=25", tgAPI, p.token, offset)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Result, nil
}

func (p *Plugin) send(ctx context.Context, chatID, text string) error {
	payload, _ := json.Marshal(map[string]string{"chat_id": chatID, "text": text})
	url := fmt.Sprintf("%s%s/sendMessage", tgAPI, p.token)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func boolVal(m map[string]interface{}, k string) bool {
	v, ok := m[k]
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	default:
		return false
	}
}
