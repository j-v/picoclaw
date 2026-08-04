package discord

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/sipeed/picoclaw/pkg/audio/tts"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

type stubTTSProvider struct{}

func (stubTTSProvider) Name() string { return "stub-tts" }

func (stubTTSProvider) Synthesize(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(&noopReader{}), nil
}

type noopReader struct{}

func (*noopReader) Read(p []byte) (int, error) {
	return 0, io.EOF
}

// newTestDiscordSession returns a discordgo session whose state cache has a
// registered guild. The forked discordgo's State.ChannelAdd refuses channels
// whose GuildID is not present in the guild cache, so tests that seed the
// state cache must register a guild first and set GuildID on their channels.
func newTestDiscordSession(t *testing.T) *discordgo.Session {
	t.Helper()
	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}
	if err := session.State.GuildAdd(&discordgo.Guild{ID: "G001"}); err != nil {
		t.Fatalf("GuildAdd() error: %v", err)
	}
	return session
}

func TestApplyDiscordProxy_CustomProxy(t *testing.T) {
	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}

	if err = applyDiscordProxy(session, "http://127.0.0.1:7890"); err != nil {
		t.Fatalf("applyDiscordProxy() error: %v", err)
	}

	req, err := http.NewRequest("GET", "https://discord.com/api/v10/gateway", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error: %v", err)
	}

	restProxy := session.Client.Transport.(*http.Transport).Proxy
	restProxyURL, err := restProxy(req)
	if err != nil {
		t.Fatalf("rest proxy func error: %v", err)
	}
	if got, want := restProxyURL.String(), "http://127.0.0.1:7890"; got != want {
		t.Fatalf("REST proxy = %q, want %q", got, want)
	}

	wsProxyURL, err := session.Dialer.Proxy(req)
	if err != nil {
		t.Fatalf("ws proxy func error: %v", err)
	}
	if got, want := wsProxyURL.String(), "http://127.0.0.1:7890"; got != want {
		t.Fatalf("WS proxy = %q, want %q", got, want)
	}
}

func TestApplyDiscordProxy_FromEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8888")
	t.Setenv("http_proxy", "http://127.0.0.1:8888")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8888")
	t.Setenv("https_proxy", "http://127.0.0.1:8888")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("all_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}

	if err = applyDiscordProxy(session, ""); err != nil {
		t.Fatalf("applyDiscordProxy() error: %v", err)
	}

	req, err := http.NewRequest("GET", "https://discord.com/api/v10/gateway", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error: %v", err)
	}

	gotURL, err := session.Dialer.Proxy(req)
	if err != nil {
		t.Fatalf("ws proxy func error: %v", err)
	}

	wantURL, err := url.Parse("http://127.0.0.1:8888")
	if err != nil {
		t.Fatalf("url.Parse() error: %v", err)
	}
	if gotURL.String() != wantURL.String() {
		t.Fatalf("WS proxy = %q, want %q", gotURL.String(), wantURL.String())
	}
}

func TestApplyDiscordProxy_InvalidProxyURL(t *testing.T) {
	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}

	if err = applyDiscordProxy(session, "://bad-proxy"); err == nil {
		t.Fatal("applyDiscordProxy() expected error for invalid proxy URL, got nil")
	}
}

func TestSend_NonToolFeedbackDeletesTrackedProgressMessage(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/channels/chat-1/messages/prog-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"prog-1"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	defer func() {
		discordgo.EndpointChannels = origChannels
	}()

	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}
	session.Client = server.Client()

	ch := &DiscordChannel{
		BaseChannel: channels.NewBaseChannel("discord", nil, bus.NewMessageBus(), nil),
		session:     session,
		ctx:         context.Background(),
		typingStop:  make(map[string]chan struct{}),
		voiceSSRC:   make(map[string]map[uint32]string),
	}
	ch.progress = channels.NewToolFeedbackAnimator(ch.EditMessage)
	ch.SetRunning(true)
	ch.RecordToolFeedbackMessage("chat-1", "prog-1", "🔧 `read_file`")

	ids, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "chat-1",
		Content: "final reply",
		Context: bus.InboundContext{
			Channel: "discord",
			ChatID:  "chat-1",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got, want := ids, []string{"prog-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Send() ids = %v, want %v", got, want)
	}
	if _, ok := ch.currentToolFeedbackMessage("chat-1"); ok {
		t.Fatal("expected tracked tool feedback message to be cleared")
	}

	mu.Lock()
	defer mu.Unlock()
	wantRequests := []string{
		"PATCH /channels/chat-1/messages/prog-1",
	}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestEditMessage_UsesContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg-1"}`)
		}
	}))
	defer server.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	defer func() {
		discordgo.EndpointChannels = origChannels
	}()

	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}
	session.Client = server.Client()

	ch := &DiscordChannel{
		BaseChannel: channels.NewBaseChannel("discord", nil, bus.NewMessageBus(), nil),
		session:     session,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = ch.EditMessage(ctx, "chat-1", "msg-1", "still running")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected EditMessage() to fail when context times out")
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("EditMessage() ignored context timeout, elapsed=%v", elapsed)
	}
}

func TestFinalizeTrackedToolFeedbackMessage_StopsTrackingBeforeEdit(t *testing.T) {
	ch := &DiscordChannel{
		progress: channels.NewToolFeedbackAnimator(nil),
	}
	ch.RecordToolFeedbackMessage("chat-1", "msg-1", "🔧 `read_file`")

	msgIDs, handled := ch.finalizeTrackedToolFeedbackMessage(
		context.Background(),
		"chat-1",
		"final reply",
		func(_ context.Context, chatID, messageID, content string) error {
			if _, ok := ch.currentToolFeedbackMessage(chatID); ok {
				t.Fatal("expected tracked tool feedback to be stopped before edit")
			}
			if chatID != "chat-1" || messageID != "msg-1" || content != "final reply" {
				t.Fatalf("unexpected edit args: %s %s %s", chatID, messageID, content)
			}
			return nil
		},
	)
	if !handled {
		t.Fatal("expected finalizeTrackedToolFeedbackMessage to handle tracked message")
	}
	if got, want := msgIDs, []string{"msg-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("finalizeTrackedToolFeedbackMessage() ids = %v, want %v", got, want)
	}
}

func TestSend_NonToolFeedbackFinalizerStillStartsTTS(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/channels/chat-1/messages/prog-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"prog-1"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	defer func() {
		discordgo.EndpointChannels = origChannels
	}()

	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New() error: %v", err)
	}
	session.Client = server.Client()

	ttsStarted := make(chan string, 1)
	ch := &DiscordChannel{
		BaseChannel: channels.NewBaseChannel("discord", nil, bus.NewMessageBus(), nil),
		session:     session,
		ctx:         context.Background(),
		typingStop:  make(map[string]chan struct{}),
		voiceSSRC:   make(map[string]map[uint32]string),
		tts:         tts.TTSProvider(stubTTSProvider{}),
	}
	ch.ttsVoiceFn = func(string) (*discordgo.VoiceConnection, bool) {
		return &discordgo.VoiceConnection{}, true
	}
	ch.playTTSFn = func(_ context.Context, _ *discordgo.VoiceConnection, text string, _ uint64) {
		ttsStarted <- text
	}
	ch.progress = channels.NewToolFeedbackAnimator(ch.EditMessage)
	ch.SetRunning(true)
	ch.RecordToolFeedbackMessage("chat-1", "prog-1", "🔧 `read_file`")

	ids, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "chat-1",
		Content: "final reply",
		Context: bus.InboundContext{
			Channel: "discord",
			ChatID:  "chat-1",
		},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got, want := ids, []string{"prog-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Send() ids = %v, want %v", got, want)
	}

	select {
	case got := <-ttsStarted:
		if got != "final reply" {
			t.Fatalf("TTS content = %q, want final reply", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected TTS to start for finalized tracked tool feedback reply")
	}
}

func TestResolveThreadParentID_StateCache(t *testing.T) {
	session := newTestDiscordSession(t)

	// Thread channel with a parent.
	if err := session.State.ChannelAdd(&discordgo.Channel{
		ID:       "111222333",
		GuildID:  "G001",
		Type:     discordgo.ChannelTypeGuildPublicThread,
		ParentID: "999888777",
	}); err != nil {
		t.Fatalf("ChannelAdd(thread) error: %v", err)
	}
	// Regular (non-thread) channel.
	if err := session.State.ChannelAdd(&discordgo.Channel{
		ID:      "555666777",
		GuildID: "G001",
		Type:    discordgo.ChannelTypeGuildText,
	}); err != nil {
		t.Fatalf("ChannelAdd(text) error: %v", err)
	}

	ch := &DiscordChannel{session: session}

	if got, want := ch.resolveThreadParentID(session, "111222333"), "999888777"; got != want {
		t.Fatalf("resolveThreadParentID(thread) = %q, want %q", got, want)
	}
	if got := ch.resolveThreadParentID(session, "555666777"); got != "" {
		t.Fatalf("resolveThreadParentID(text) = %q, want empty", got)
	}
	if got := ch.resolveThreadParentID(session, "000000000"); got != "" {
		t.Fatalf("resolveThreadParentID(unknown) = %q, want empty", got)
	}
}

func TestMaybeResolveThreadParent(t *testing.T) {
	session := newTestDiscordSession(t)

	if err := session.State.ChannelAdd(&discordgo.Channel{
		ID:       "111222333",
		GuildID:  "G001",
		Type:     discordgo.ChannelTypeGuildPublicThread,
		ParentID: "999888777",
	}); err != nil {
		t.Fatalf("ChannelAdd(thread) error: %v", err)
	}
	if err := session.State.ChannelAdd(&discordgo.Channel{
		ID:      "555666777",
		GuildID: "G001",
		Type:    discordgo.ChannelTypeGuildText,
	}); err != nil {
		t.Fatalf("ChannelAdd(text) error: %v", err)
	}

	// Flag off: never resolves, even for thread messages.
	ch := &DiscordChannel{session: session, config: &config.DiscordSettings{}}
	if got := ch.maybeResolveThreadParent(session, "G001", "111222333"); got != "" {
		t.Fatalf("flag off: maybeResolveThreadParent = %q, want empty", got)
	}

	// Flag on, guild message in a thread -> parent channel ID.
	ch.config.ThreadParentRouting = true
	if got, want := ch.maybeResolveThreadParent(session, "G001", "111222333"), "999888777"; got != want {
		t.Fatalf("flag on thread: maybeResolveThreadParent = %q, want %q", got, want)
	}

	// Flag on, non-thread channel -> empty.
	if got := ch.maybeResolveThreadParent(session, "G001", "555666777"); got != "" {
		t.Fatalf("flag on text channel: maybeResolveThreadParent = %q, want empty", got)
	}

	// Flag on, DM (no guild) -> empty, even for a thread ID.
	if got := ch.maybeResolveThreadParent(session, "", "111222333"); got != "" {
		t.Fatalf("flag on DM: maybeResolveThreadParent = %q, want empty", got)
	}
}

func TestThreadStartMessage(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/channels/parent-1/messages/thread-1":
			w.Header().Set("Content-Type", "application/json")
			const startJSON = `{"id":"thread-1","content":"hello from the start message",` +
				`"author":{"username":"alice"}}`
			_, _ = io.WriteString(w, startJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/channels/parent-2/messages/thread-2":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"thread-2","content":"","author":{"username":"nobody"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"404: Not Found"}`)
		}
	}))
	defer server.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	defer func() {
		discordgo.EndpointChannels = origChannels
	}()

	session := newTestDiscordSession(t)
	session.Client = server.Client()

	// Register a state-cached channel so resolveDiscordRefs resolves <#id>
	// without an extra REST call.
	if err := session.State.ChannelAdd(&discordgo.Channel{
		ID:      "111222333",
		GuildID: "G001",
		Name:    "general",
	}); err != nil {
		t.Fatalf("ChannelAdd() error: %v", err)
	}

	ch := &DiscordChannel{session: session}

	// Success case: fetches the start message, resolves refs, formats it.
	got := ch.threadStartMessage(session, "G001", "parent-1", "thread-1")
	want := "[thread started from alice's message]: hello from the start message"
	if got != want {
		t.Fatalf("threadStartMessage() = %q, want %q", got, want)
	}

	// Empty content case: returns "".
	if got := ch.threadStartMessage(session, "G001", "parent-2", "thread-2"); got != "" {
		t.Fatalf("threadStartMessage(empty content) = %q, want empty", got)
	}

	// Error case (unknown parent/message -> 404): returns "".
	if got := ch.threadStartMessage(session, "G001", "parent-3", "thread-3"); got != "" {
		t.Fatalf("threadStartMessage(unknown) = %q, want empty", got)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, req := range requests {
		if req == "GET /channels/parent-1/messages/thread-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected start-message fetch request, got %v", requests)
	}
}

func TestThreadStartMessage_ResolvesChannelRefs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels/parent-1/messages/thread-1" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"thread-1","content":"see <#111222333>","author":{"username":"alice"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"404: Not Found"}`)
	}))
	defer server.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	defer func() {
		discordgo.EndpointChannels = origChannels
	}()

	session := newTestDiscordSession(t)
	session.Client = server.Client()

	if err := session.State.ChannelAdd(&discordgo.Channel{
		ID:      "111222333",
		GuildID: "G001",
		Name:    "general",
	}); err != nil {
		t.Fatalf("ChannelAdd() error: %v", err)
	}

	ch := &DiscordChannel{session: session}

	got := ch.threadStartMessage(session, "G001", "parent-1", "thread-1")
	want := "[thread started from alice's message]: see #general"
	if got != want {
		t.Fatalf("threadStartMessage() = %q, want %q", got, want)
	}
}
