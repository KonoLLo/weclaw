package messaging

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fastclaw-ai/weclaw/ilink"
)

const (
	cdnBaseURL            = "https://novac2c.cdn.weixin.qq.com/c2c"
	mediaDownloadMaxBytes = 100 << 20
)

type mediaTimeoutSettings struct {
	total time.Duration
	idle  time.Duration
}

var mediaTimeouts atomic.Value

func init() {
	mediaTimeouts.Store(mediaTimeoutSettings{total: 120 * time.Second, idle: 30 * time.Second})
}

// ConfigureMediaDownload sets the total and idle timeouts for inbound media
// downloads. Non-positive values keep the defaults (120s total, 30s idle).
func ConfigureMediaDownload(totalMS, idleMS int) {
	total := 120 * time.Second
	if totalMS > 0 {
		total = time.Duration(totalMS) * time.Millisecond
	}
	idle := 30 * time.Second
	if idleMS > 0 {
		idle = time.Duration(idleMS) * time.Millisecond
	}
	mediaTimeouts.Store(mediaTimeoutSettings{total: total, idle: idle})
	log.Printf("[media] download timeouts: total=%s idle=%s", total, idle)
}

func currentMediaTimeouts() mediaTimeoutSettings {
	if v, ok := mediaTimeouts.Load().(mediaTimeoutSettings); ok {
		return v
	}
	return mediaTimeoutSettings{total: 120 * time.Second, idle: 30 * time.Second}
}

// UploadedFile holds the result of a CDN upload.
type UploadedFile struct {
	DownloadParam string // encrypted query param for download
	AESKeyHex     string // hex-encoded AES key
	FileSize      int    // plaintext size
	CipherSize    int    // ciphertext size
}

// UploadFileToCDN encrypts and uploads a file to the WeChat CDN.
func UploadFileToCDN(ctx context.Context, client *ilink.Client, data []byte, toUserID string, mediaType int) (*UploadedFile, error) {
	// Generate random filekey and AES key
	filekey := make([]byte, 16)
	aeskey := make([]byte, 16)
	if _, err := rand.Read(filekey); err != nil {
		return nil, fmt.Errorf("generate filekey: %w", err)
	}
	if _, err := rand.Read(aeskey); err != nil {
		return nil, fmt.Errorf("generate aeskey: %w", err)
	}

	filekeyHex := hex.EncodeToString(filekey)
	aeskeyHex := hex.EncodeToString(aeskey)

	// Calculate MD5 of plaintext
	hash := md5.Sum(data)
	rawMD5 := hex.EncodeToString(hash[:])

	// Calculate ciphertext size (PKCS7 padding)
	cipherSize := aesECBPaddedSize(len(data))

	// Get upload URL from iLink API
	uploadReq := &ilink.GetUploadURLRequest{
		FileKey:     filekeyHex,
		MediaType:   mediaType,
		ToUserID:    toUserID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5,
		FileSize:    cipherSize,
		NoNeedThumb: true,
		AESKey:      aeskeyHex,
		BaseInfo:    ilink.BuildBaseInfo(),
	}

	uploadResp, err := client.GetUploadURL(ctx, uploadReq)
	if err != nil {
		return nil, fmt.Errorf("get upload URL: %w", err)
	}
	if uploadResp.Ret != 0 {
		return nil, fmt.Errorf("get upload URL failed: ret=%d errmsg=%s", uploadResp.Ret, uploadResp.ErrMsg)
	}

	// Encrypt data with AES-128-ECB
	encrypted, err := encryptAESECB(data, aeskey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	// Upload to CDN: prefer server-provided full URL, fall back to param-based construction
	cdnURL := strings.TrimSpace(uploadResp.UploadFullURL)
	if cdnURL == "" {
		if uploadResp.UploadParam == "" {
			return nil, fmt.Errorf("getuploadurl returned no upload URL (need upload_full_url or upload_param)")
		}
		cdnURL = fmt.Sprintf("%s/upload?encrypted_query_param=%s&filekey=%s",
			cdnBaseURL, url.QueryEscape(uploadResp.UploadParam), url.QueryEscape(filekeyHex))
	}

	downloadParam, err := uploadToCDN(ctx, encrypted, cdnURL)
	if err != nil {
		return nil, fmt.Errorf("CDN upload: %w", err)
	}

	return &UploadedFile{
		DownloadParam: downloadParam,
		AESKeyHex:     aeskeyHex,
		FileSize:      len(data),
		CipherSize:    cipherSize,
	}, nil
}

// AESKeyToBase64 converts a hex AES key to base64 format for message items.
func AESKeyToBase64(hexKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(hexKey))
}

// DownloadFileFromCDN downloads and decrypts a file from the WeChat CDN using
// the legacy encrypt_query_param URL.
func DownloadFileFromCDN(ctx context.Context, encryptQueryParam, aesKeyBase64 string) ([]byte, error) {
	return DownloadCDNMedia(ctx, "", encryptQueryParam, aesKeyBase64)
}

// DownloadCDNMedia downloads media from full_url when present, otherwise builds
// the legacy download URL. An empty AES key means the payload is plaintext.
func DownloadCDNMedia(ctx context.Context, fullURL, encryptQueryParam, aesKeyBase64 string) ([]byte, error) {
	url, err := cdnDownloadURL(fullURL, encryptQueryParam)
	if err != nil {
		return nil, err
	}
	raw, err := downloadCDNBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(aesKeyBase64) == "" {
		return raw, nil
	}
	aesKey, err := parseAESKey(aesKeyBase64)
	if err != nil {
		return nil, err
	}
	return decryptAESECB(raw, aesKey)
}

// DownloadMediaInfo resolves all CDN variants used by inbound message items.
// imageAESKeyHex is the hex key from image_item.aeskey, which takes precedence
// over media.aes_key; images without any key are downloaded as plaintext.
func DownloadMediaInfo(ctx context.Context, media *ilink.MediaInfo, imageAESKeyHex string) ([]byte, error) {
	if media == nil {
		return nil, fmt.Errorf("missing media info")
	}
	if media.FullURL == "" && media.EncryptQueryParam == "" {
		return nil, fmt.Errorf("missing media URL")
	}
	key := strings.TrimSpace(imageAESKeyHex)
	if key == "" {
		key = media.AESKey
	}
	return DownloadCDNMedia(ctx, media.FullURL, media.EncryptQueryParam, key)
}

func cdnDownloadURL(fullURL, encryptQueryParam string) (string, error) {
	if u := strings.TrimSpace(fullURL); u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return "", fmt.Errorf("invalid media full_url")
		}
		return u, nil
	}
	if encryptQueryParam == "" {
		return "", fmt.Errorf("missing media URL")
	}
	return fmt.Sprintf("%s/download?encrypted_query_param=%s",
		cdnBaseURL, url.QueryEscape(encryptQueryParam)), nil
}

var aesKeyHexPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// parseAESKey accepts the two wire encodings: base64(raw 16 bytes) for images,
// and base64(hex string) for files/voice/video. A bare 32-char hex key is also
// accepted for image_item.aeskey.
func parseAESKey(aesKeyBase64 string) ([]byte, error) {
	raw := strings.TrimSpace(aesKeyBase64)
	if aesKeyHexPattern.MatchString(raw) {
		return hex.DecodeString(raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode AES key base64: %w", err)
	}
	if len(decoded) == aes.BlockSize {
		return decoded, nil
	}
	if len(decoded) == 32 && aesKeyHexPattern.Match(decoded) {
		return hex.DecodeString(string(decoded))
	}
	return nil, fmt.Errorf("AES key must decode to 16 bytes or a 32-char hex string")
}

func downloadCDNBytes(ctx context.Context, rawURL string) ([]byte, error) {
	timeouts := currentMediaTimeouts()
	reqCtx, cancel := context.WithTimeout(ctx, timeouts.total)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download from CDN: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("CDN download HTTP %d: %s", resp.StatusCode, string(body))
	}

	data, err := readAllWithIdleTimeout(resp.Body, timeouts.idle)
	if err != nil {
		return nil, fmt.Errorf("read CDN response: %w", err)
	}
	return data, nil
}

// readAllWithIdleTimeout aborts a stalled response body. Closing the body on
// timeout also releases the read goroutine.
func readAllWithIdleTimeout(body io.ReadCloser, idle time.Duration) ([]byte, error) {
	type readResult struct {
		data []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, mediaDownloadMaxBytes+1))
		done <- readResult{data: data, err: err}
	}()

	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		if len(res.data) > mediaDownloadMaxBytes {
			return nil, fmt.Errorf("media exceeds %d bytes", mediaDownloadMaxBytes)
		}
		return res.data, nil
	case <-timer.C:
		_ = body.Close()
		return nil, fmt.Errorf("idle timeout after %s", idle)
	}
}

// decryptAESECB decrypts data encrypted with AES-128-ECB and removes PKCS7 padding.
func decryptAESECB(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext is not a multiple of block size")
	}

	plaintext := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plaintext[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}

	// Remove PKCS7 padding
	if len(plaintext) == 0 {
		return plaintext, nil
	}
	padLen := int(plaintext[len(plaintext)-1])
	if padLen > aes.BlockSize || padLen == 0 {
		return nil, fmt.Errorf("invalid PKCS7 padding")
	}
	return plaintext[:len(plaintext)-padLen], nil
}

func uploadToCDN(ctx context.Context, encrypted []byte, cdnURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cdnURL, bytes.NewReader(encrypted))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("CDN upload HTTP %d: %s", resp.StatusCode, string(body))
	}

	downloadParam := resp.Header.Get("X-Encrypted-Param")
	if downloadParam == "" {
		return "", fmt.Errorf("CDN upload: missing X-Encrypted-Param header")
	}

	return downloadParam, nil
}

// encryptAESECB encrypts data using AES-128-ECB with PKCS7 padding.
func encryptAESECB(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// PKCS7 padding
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}

	// ECB mode: encrypt each block independently
	encrypted := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(encrypted[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}

	return encrypted, nil
}

func aesECBPaddedSize(plaintextSize int) int {
	return (plaintextSize/aes.BlockSize + 1) * aes.BlockSize
}
