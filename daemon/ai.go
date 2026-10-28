package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type AICenter struct {
	config      *Config
	client      *http.Client
	mu          sync.Mutex
	history     []ChatMessage
	lastActivity time.Time
}

func NewAICenter(cfg *Config) *AICenter {
	var transport *http.Transport

	if cfg.ByeDPIEnabled {
		dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.ByeDPIPort), nil, proxy.Direct)
		if err == nil {
			transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				},
			}
		}
	}

	if transport == nil {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	return &AICenter{
		config: cfg,
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: transport,
		},
	}
}

// Transcribe sends the recorded WAV to Groq Whisper.
func (ai *AICenter) Transcribe(wavBytes []byte) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="speech.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, err := writer.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wavBytes); err != nil {
		return "", err
	}

	_ = writer.WriteField("model", ai.config.STTModel)
	_ = writer.WriteField("language", "ru")
	_ = writer.WriteField("response_format", "json")
	_ = writer.WriteField("temperature", "0.0")
	_ = writer.WriteField("prompt", "Джарвис, время, погода, музыка, стоп, громкость, включи, выключи")
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", ai.config.GroqBaseURL+"/audio/transcriptions", bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(body.Len())
	req.Header.Set("Authorization", "Bearer "+ai.config.GroqAPIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := ai.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return "", err
	}

	raw := strings.TrimSpace(res.Text)
	lower := strings.ToLower(raw)
	// Suppress common Whisper phantom hallucinations on silence
	if strings.Contains(lower, "субтитр") || strings.Contains(lower, "dimatorzok") || strings.Contains(lower, "редактор") || strings.Contains(lower, "перевод") || raw == "." {
		log.Printf("[STT] Filtered Whisper silence hallucination: %q", raw)
		return "", nil
	}
	return raw, nil
}

// CheckWakeWord verifies that speech addresses the assistant directly and filters out
// casual mentions in conversation (e.g. "у Алисы", "с Алисой", or wake word buried mid-sentence).
func (ai *AICenter) CheckWakeWord(text string) (bool, string) {
	lower := strings.ToLower(text)
	lower = strings.Trim(lower, " .,!?;:-_«»\"'…")

	// Preposition rejection: If wake word is preceded by a preposition, it's a conversation reference.
	prepRegex := regexp.MustCompile(`\b(у|с|со|об|про|о|для|от|к|как|что|где|про|говорили|спросил|сказал)\s+(?:алис[а-я]*|яндекс[а-я]*)`)
	if prepRegex.MatchString(lower) {
		return false, text
	}

	// Split into words
	words := strings.Fields(lower)
	if len(words) == 0 {
		return false, text
	}

	// Allowed opening greetings before the wake word
	salutations := map[string]bool{
		"привет": true, "слушай": true, "эй": true, "хэй": true,
		"здравствуй": true, "здравствуйте": true, "ок": true,
	}

	var wakeWordMatched bool
	var queryStartIdx int

	// Case 1: First word is directly a wake word
	for _, ww := range ai.config.WakeWords {
		if isWakeWordToken(words[0], ww) {
			wakeWordMatched = true
			queryStartIdx = 1
			break
		}
	}

	// Case 2: First word is a greeting, and second word is a wake word
	if !wakeWordMatched && len(words) >= 2 && salutations[strings.Trim(words[0], ",.!?:")] {
		for _, ww := range ai.config.WakeWords {
			if isWakeWordToken(words[1], ww) {
				wakeWordMatched = true
				queryStartIdx = 2
				break
			}
		}
	}

	// If not addressed at the start with a direct invocation, reject mid-conversation mentions
	if !wakeWordMatched {
		return false, text
	}

	// Extract remaining query
	if queryStartIdx >= len(words) {
		return true, ""
	}

	// Reconstruct the remaining query cleanly
	queryWords := words[queryStartIdx:]
	query := strings.Join(queryWords, " ")
	query = strings.Trim(query, " .,!?;:-_«»\"'…")

	return true, query
}

// isWakeWordToken ensures the token strictly matches the wake word ("алиса", "алис")
// and rejects grammatical inflections used in conversation ("алисой", "алису", "алисе").
func isWakeWordToken(token, wakeWord string) bool {
	clean := strings.Trim(token, " .,!?;:-_«»\"'…")
	if clean == wakeWord {
		return true
	}
	if wakeWord == "алиса" && clean == "алис" {
		return true
	}
	return false
}

type LLMResponse struct {
	Intent string
	Param  string
	Answer string
}

// AskLLM sends prompt to Groq Chat Completions with session context memory.
func (ai *AICenter) AskLLM(prompt string) (*LLMResponse, error) {
	ai.mu.Lock()
	// Reset context if idle for more than 60 seconds
	if time.Since(ai.lastActivity) > 60*time.Second {
		ai.history = nil
	}
	ai.lastActivity = time.Now()

	loc := time.FixedZone("MSK", ai.config.TimezoneOffsetHours*3600)
	currentTime := time.Now().In(loc).Format("15:04, 02.01.2006")
	sysPrompt := fmt.Sprintf("Ты — голосовой AI-ассистент на умной колонке. Текущее точное время и дата: %s.\n"+
		"Отвечай кратко, естественно, честно и по делу, строго в 1-2 предложениях (для озвучки голосом).\n"+
		"Не скрывай свою настоящую модель и архитектуру, если спрашивают.\n"+
		"Если просят включить музыку: начни с 'MUSIC: <запрос>'\n"+
		"Если просят остановить: начни с 'STOP'", currentTime)

	var messages []map[string]string
	messages = append(messages, map[string]string{"role": "system", "content": sysPrompt})

	// Add recent session history (last 4 turns)
	start := 0
	if len(ai.history) > 4 {
		start = len(ai.history) - 4
	}
	for _, m := range ai.history[start:] {
		messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
	}

	// Append current user prompt
	messages = append(messages, map[string]string{"role": "user", "content": prompt})
	ai.mu.Unlock()

	reqBody := map[string]interface{}{
		"model":       ai.config.LLMModel,
		"messages":    messages,
		"temperature": 0.4,
		"max_tokens":  250,
	}

	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", ai.config.GroqBaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Authorization", "Bearer "+ai.config.GroqAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := ai.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		return nil, err
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("empty LLM response")
	}

	rawAnswer := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	if rawAnswer == "" {
		rawAnswer = "Я вас слушаю. Чем могу помочь?"
	}

	// Save to session history
	ai.mu.Lock()
	ai.history = append(ai.history, ChatMessage{Role: "user", Content: prompt})
	ai.history = append(ai.history, ChatMessage{Role: "assistant", Content: rawAnswer})
	if len(ai.history) > 6 {
		ai.history = ai.history[len(ai.history)-6:]
	}
	ai.mu.Unlock()

	res := &LLMResponse{Intent: "TEXT", Answer: rawAnswer}
	if strings.HasPrefix(rawAnswer, "MUSIC:") {
		res.Intent = "MUSIC"
		res.Param = strings.TrimSpace(strings.TrimPrefix(rawAnswer, "MUSIC:"))
		res.Answer = fmt.Sprintf("Включаю: %s", res.Param)
	} else if strings.HasPrefix(rawAnswer, "STOP") {
		res.Intent = "STOP"
		res.Answer = "Останавливаю."
	}

	return res, nil
}
