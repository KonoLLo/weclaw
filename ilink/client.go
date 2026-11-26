package ilink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultBaseURL  = "https://ilinkai.weixin.qq.com"
	channelVersion  = "2.4.4"
	defaultBotAgent = "WeClaw/" + channelVersion
	longPollTimeout = 35 * time.Second
	sendTimeout     = 15 * time.Second
	notifyTimeout   = 5 * time.Second
)

type channelOptions struct {
	botAgent string
	routeTag int64
}

var currentChannelOptions atomic.Value

func init() {
	currentChannelOptions.Store(channelOptions{botAgent: defaultBotAgent})
}

// ConfigureChannel applies the optional bot_agent identity and SKRouteTag used
// by every iLink request. Invalid bot_agent values fall back to the default.
func ConfigureChannel(botAgent string, routeTag int64) {
	opts := channelOptions{botAgent: sanitizeBotAgent(botAgent), routeTag: routeTag}
	currentChannelOptions.Store(opts)
	log.Printf("[ilink] channel identity: bot_agent=%q route_tag=%d", opts.botAgent, opts.routeTag)
}

func sanitizeBotAgent(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultBotAgent
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw) && len(out) < 256; i++ {
		c := raw[i]
		if c >= 0x20 && c <= 0x7e {
			out = append(out, c)
		}
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return defaultBotAgent
	}
	return value
}

func channelConfig() channelOptions {
	if opts, ok := currentChannelOptions.Load().(channelOptions); ok {
		return opts
	}
	return channelOptions{botAgent: defaultBotAgent}
}

// Client is an iLink HTTP API client.
type Client struct {
	baseURL    string
	botToken   string
	botID      string
	httpClient *http.Client
	wechatUIN  string
}

// NewClient creates a new iLink API client.
func NewClient(creds *Credentials) *Client {
	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL:    baseURL,
		botToken:   creds.BotToken,
		botID:      creds.ILinkBotID,
		httpClient: &http.Client{},
		wechatUIN:  generateWechatUIN(),
	}
}

// NewUnauthenticatedClient creates a client without credentials for login flow.
func NewUnauthenticatedClient() *Client {
	return &Client{
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: 40 * time.Second},
		wechatUIN:  generateWechatUIN(),
	}
}

// BotID returns the bot's user ID.
func (c *Client) BotID() string {
	return c.botID
}

// GetUpdates performs a long-poll for new messages.
func (c *Client) GetUpdates(ctx context.Context, buf string) (*GetUpdatesResponse, error) {
	reqBody := GetUpdatesRequest{
		GetUpdatesBuf: buf,
		BaseInfo:      BuildBaseInfo(),
	}

	ctx, cancel := context.WithTimeout(ctx, longPollTimeout+5*time.Second)
	defer cancel()

	var resp GetUpdatesResponse
	if err := c.doPost(ctx, "/ilink/bot/getupdates", reqBody, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SendMessage sends a message through iLink.
func (c *Client) SendMessage(ctx context.Context, msg *SendMessageRequest) (*SendMessageResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	var resp SendMessageResponse
	if err := c.doPost(ctx, "/ilink/bot/sendmessage", msg, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetConfig fetches bot config for a user (includes typing_ticket).
func (c *Client) GetConfig(ctx context.Context, userID, contextToken string) (*GetConfigResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req := GetConfigRequest{
		ILinkUserID:  userID,
		ContextToken: contextToken,
		BaseInfo:     BuildBaseInfo(),
	}

	var resp GetConfigResponse
	if err := c.doPost(ctx, "/ilink/bot/getconfig", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SendTyping sends a typing indicator to a user.
func (c *Client) SendTyping(ctx context.Context, userID, typingTicket string, status int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req := SendTypingRequest{
		ILinkUserID:  userID,
		TypingTicket: typingTicket,
		Status:       status,
		BaseInfo:     BuildBaseInfo(),
	}

	var resp SendTypingResponse
	if err := c.doPost(ctx, "/ilink/bot/sendtyping", req, &resp); err != nil {
		return err
	}
	if resp.Ret != 0 {
		return fmt.Errorf("sendtyping failed: ret=%d errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	return nil
}

// GetUploadURL gets a pre-signed CDN upload URL for media files.
func (c *Client) GetUploadURL(ctx context.Context, req *GetUploadURLRequest) (*GetUploadURLResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req.BaseInfo = BuildBaseInfo()

	var resp GetUploadURLResponse
	if err := c.doPost(ctx, "/ilink/bot/getuploadurl", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BuildBaseInfo returns request metadata compatible with openclaw-weixin 2.4.x.
func BuildBaseInfo() BaseInfo {
	return BaseInfo{
		ChannelVersion: channelVersion,
		BotAgent:       channelConfig().botAgent,
	}
}

// NotifyStart announces that the bot is online. Failures are advisory only.
func (c *Client) NotifyStart(ctx context.Context) error {
	return c.notify(ctx, "/ilink/bot/msg/notifystart")
}

// NotifyStop announces that the bot is going offline. Failures are advisory only.
func (c *Client) NotifyStop(ctx context.Context) error {
	return c.notify(ctx, "/ilink/bot/msg/notifystop")
}

type notifyResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

func (c *Client) notify(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	var resp notifyResponse
	if err := c.doPost(ctx, path, struct {
		BaseInfo BaseInfo `json:"base_info"`
	}{BaseInfo: BuildBaseInfo()}, &resp); err != nil {
		return err
	}
	if resp.Ret != 0 {
		return fmt.Errorf("%s failed: ret=%d errmsg=%s", path, resp.Ret, resp.ErrMsg)
	}
	return nil
}

// BaseURL returns the base URL for CDN operations.
func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) doPost(ctx context.Context, path string, body interface{}, result interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, withRouteTag(c.baseURL+path), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

func (c *Client) doGet(ctx context.Context, url string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, withRouteTag(url), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	req.Header.Set("X-WECHAT-UIN", c.wechatUIN)
	if tag := channelConfig().routeTag; tag != 0 {
		req.Header.Set("SKRouteTag", strconv.FormatInt(tag, 10))
	}
}

func withRouteTag(rawURL string) string {
	tag := channelConfig().routeTag
	if tag == 0 {
		return rawURL
	}
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return rawURL + separator + "_cradle_hostname=g" + strconv.FormatInt(tag, 10)
}

func generateWechatUIN() string {
	var n uint32
	_ = binary.Read(rand.Reader, binary.LittleEndian, &n)
	s := fmt.Sprintf("%d", n)
	return base64.StdEncoding.EncodeToString([]byte(s))
}
